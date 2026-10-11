"""Authenticated record/source resolution; no deployment authority.

The concrete transport owns signature verification. The immutable projections
below retain that operation's inputs, not a transferable cryptographic receipt.
Plain selection does not invoke rollback policy or fabricate a live catalog.
Only the reviewed v0 source format and authenticated v1-prefix reader0 exist.
"""
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import record_policy as policy
import record_transport as transport
import release_record as records
import schema_change
import ship_common as common
import source_git
import pending_source
import contract_authorization as contract
import contract_collect

_CATALOG_PINS = dict(policy.V0_PINS)
_CATALOG_PATHS = tuple(sorted(_CATALOG_PINS))
_WEB_RECIPE = "docker/release/web.Dockerfile"
_DRILLS = "release/deployment/drills.json"
MAX_CHANGED_PATHS = 10_000
MAX_AUDIT_BYTES = 512 * 1024


@dataclass(frozen=True, slots=True)
class ReleaseSelection:
    index: int
    release: str
    record_sha: str
    manifest_sha256: str
    images: tuple[tuple[str, str], ...]
    actual_web_source_sha: str
    reader: policy.SourceFacts
    live: policy.LiveContract | None


@dataclass(frozen=True, slots=True)
class ResolvedChain:
    chain: policy.PolicyChain
    inventory: transport.InventorySnapshot
    image_bindings: tuple[transport.VerifiedImages, ...]
    latest: ReleaseSelection
    selected: ReleaseSelection


@dataclass(frozen=True, slots=True)
class BlobObservation:
    path: str
    raw: bytes | None


@dataclass(frozen=True, slots=True)
class CatalogMaterial:
    candidate_sha: str
    candidate_blobs: tuple[policy.SourceBlob, ...]
    baseline_sha: str
    baseline_provenance: str
    baseline_blobs: tuple[BlobObservation, ...]


@dataclass(frozen=True, slots=True)
class WebRecipe:
    source_sha: str
    path: str
    raw: bytes
    sha256: str


@dataclass(frozen=True, slots=True)
class ChangedPath:
    status: str
    path: str


@dataclass(frozen=True, slots=True)
class ChangedRange:
    roles: tuple[str, ...]
    base_sha: str
    head_sha: str
    rows: tuple[ChangedPath, ...]


@dataclass(frozen=True, slots=True)
class ForwardAuditMaterial:
    """Complete source observations, not drill/label/pending authorization."""
    candidate_sha: str
    drills: BlobObservation
    changes: tuple[ChangedRange, ...]


@dataclass(frozen=True, slots=True)
class ForwardResolution:
    resolved: ResolvedChain
    candidate: policy.SourceFacts
    classification: schema_change.PolicyClassification
    workflow: bytes
    workflow_sha256: str
    previous_web_source_sha: str
    database_base_source: str
    catalog_material: CatalogMaterial
    previous_recipe: WebRecipe
    # None is exclusively historical codec-v1 custody, never complete audit
    # evidence. Every concrete forward source operation below supplies it.
    audit: ForwardAuditMaterial | None = None
    # Historical custody can lack the observation. A concrete forward result
    # always has the complete raw inventory, including explicit empty tuples.
    # A blocked preview can retain captured invalid source bytes; consumers
    # must replay them, not treat this type alone as validated plan authority.
    pending: pending_source.PendingMaterial | None = None


@dataclass(frozen=True, slots=True)
class ForwardAdmission:
    """Exact-main source observation, not semantic or deployment permission."""
    resolution: ForwardResolution
    contract_observation: contract.ContractObservation | None


@dataclass(frozen=True, slots=True)
class RollbackResolution:
    """Source-bound transition data, never permission to mutate a provider.

    Before the first measured v2 promotion, legacy_chain contains only genuine
    authenticated v1 documents and retains the old rollback guard. Thereafter
    it is absent and decision retains the current database origin and reader
    floor. The two routes cannot be mixed or used to append v1 after v2.
    """
    resolved: ResolvedChain
    legacy_chain: records.Chain | None
    legacy_classification: schema_change.Classification | None
    decision: policy.RollbackDecision | None


@dataclass(frozen=True, slots=True)
class RollbackSourceMaterial:
    source_sha: str
    provenance: str
    blobs: tuple[BlobObservation, ...]


@dataclass(frozen=True, slots=True)
class RollbackProofResolution:
    """Preparatory exact-span source data, not compatible or deployable proof.

    No producer, Actions qualification, provider observation or runtime command
    contract is established here. The operational rollback route still refuses
    multiple non-none intermediates. Raw material never supplies row contents.
    """
    resolved: ResolvedChain
    requirements: policy.RollbackRequirements
    producer: policy.SourceFacts
    workflow: bytes
    workflow_sha256: str
    materials: tuple[RollbackSourceMaterial, ...]
    target_recipe: WebRecipe
    live_recipe: WebRecipe


def rollback_material_digest(resolution: RollbackProofResolution) -> str:
    """Commit exact bounded raw observations; never authenticate or admit them.

    The producer must first apply the complete source/catalog validator. This
    serializer cannot establish material completeness, current-v0 eligibility,
    ancestry, freshness, or the authority for an observed absence. Empty bytes
    and null have distinct commitments; only that stronger validator can admit
    either. Both recipe roles remain present when their observations are equal.
    """
    def need(value):
        if not value:
            raise policy.PolicyRefusal("rollback-material-invalid")

    def valid_sha(value):
        return type(value) is str and common.SHA1_RE.fullmatch(value) is not None

    need(type(resolution) is RollbackProofResolution
         and type(resolution.producer) is policy.SourceFacts
         and valid_sha(resolution.producer.sha)
         and type(resolution.resolved) is ResolvedChain)
    selected, latest = resolution.resolved.selected, resolution.resolved.latest
    need(type(selected) is ReleaseSelection and type(latest) is ReleaseSelection
         and valid_sha(selected.actual_web_source_sha) and valid_sha(latest.actual_web_source_sha))
    total = 0

    def raw_value(raw, *, nonempty=False):
        nonlocal total
        need(type(raw) is bytes and len(raw) <= policy.MAX_SOURCE_FILE_BYTES
             and (not nonempty or len(raw) > 0))
        total += len(raw)
        need(total <= transport.MAX_BYTES)
        return {"bytes": len(raw), "sha256": common.sha256_bytes(raw)}

    workflow = raw_value(resolution.workflow, nonempty=True)
    need(type(resolution.workflow_sha256) is str
         and common.SHA256_RE.fullmatch(resolution.workflow_sha256) is not None
         and workflow["sha256"] == resolution.workflow_sha256)
    workflow["path"] = ".github/workflows/compat.yml"
    need(type(resolution.materials) is tuple
         and 1 <= len(resolution.materials) <= 2 * (records.MAX_RECORDS + 1))
    materials, previous_sha = [], None
    for material in resolution.materials:
        need(type(material) is RollbackSourceMaterial and valid_sha(material.source_sha)
             and (previous_sha is None or previous_sha < material.source_sha)
             and type(material.provenance) is str
             and material.provenance in {"equal-reviewed-v0", "legacy-e001-absent-overlay"}
             and type(material.blobs) is tuple and len(material.blobs) == len(policy.SOURCE_PATHS))
        blobs, material_bytes = [], 0
        for path, blob in zip(policy.SOURCE_PATHS, material.blobs, strict=True):
            need(type(blob) is BlobObservation and type(blob.path) is str and blob.path == path)
            if blob.raw is None:
                value = None
            else:
                need(type(blob.raw) is bytes)
                material_bytes += len(blob.raw)
                need(material_bytes <= policy.MAX_SOURCE_BYTES)
                value = raw_value(blob.raw)
            blobs.append([path, value])
        materials.append({"source_sha": material.source_sha, "provenance": material.provenance, "blobs": blobs})
        previous_sha = material.source_sha
    recipes = {}
    for role, recipe, source_sha in (("target", resolution.target_recipe, selected.actual_web_source_sha),
                                     ("live", resolution.live_recipe, latest.actual_web_source_sha)):
        need(type(recipe) is WebRecipe and valid_sha(recipe.source_sha) and recipe.source_sha == source_sha
             and type(recipe.path) is str and recipe.path == _WEB_RECIPE)
        value = raw_value(recipe.raw, nonempty=True)
        need(type(recipe.sha256) is str and common.SHA256_RE.fullmatch(recipe.sha256) is not None
             and value["sha256"] == recipe.sha256)
        recipes[role] = {"source_sha": recipe.source_sha, "path": recipe.path, **value}
    payload = {"purpose": "rollback-v0-material-v1", "producer_sha": resolution.producer.sha,
               "workflow": workflow, "materials": materials, "recipes": recipes}
    return common.sha256_bytes(common.canonical_payload_bytes(payload))


def _selection(chain, index):
    entry = chain.entries[index]
    return ReleaseSelection(index, entry.release, entry.sha, entry.record.manifest_sha256,
                            entry.images, entry.record.web_source_sha, entry.reader, entry.live)


def _source_requirements(documents, images):
    """Plan every source before decoding; complete wins for a repeated SHA."""
    legacy, required, readers, commits = set(), set(), set(), set()
    seen_v2 = False
    for document, binding in zip(documents, images, strict=True):
        sources = dict(binding.source_shas)
        web = sources["web"]
        readers.add(web)
        commits.update(sources.values())
        commits.add(document["sha"])
        if document["schema_version"] == 1:
            if seen_v2:
                raise policy.PolicyRefusal("record-policy-invalid")
            legacy.add(web)
        else:
            seen_v2 = True
            if document["kind"] == "promote":
                required.add(document["sha"])
            if web not in legacy:
                required.add(web)
    return legacy, required, readers, commits


def resolve_authenticated(client: transport.ReleaseGh, repo_dir: Path,
                          git_binary: Path, selected_tag: str | None = None) -> ResolvedChain:
    """Resolve one complete chain using one shared deadline/capture allowance.

    Requires a fresh exact ReleaseGh. No supplied wire facts, sources, baseline,
    Entry objects or alternative verifier callbacks are accepted here. Testing
    seams live in the concrete transport's bounded capture, not this API.
    Future callers must independently choose forward/rollback policy and repeat
    freshness checks immediately before effects; this function enables none.
    """
    return _resolve(client, repo_dir, git_binary, selected_tag, None, forward=False)


def resolve_forward(client: transport.ReleaseGh, repo_dir: Path,
                    git_binary: Path, candidate_sha: str, *, selected_tag: str | None = None) -> ForwardResolution:
    """Gather candidate facts within the closed authenticated source operation.

    This is not a NormalRequest or deployment authorization. In particular a
    production caller must require candidate == inventory.main_sha, and the
    signed producer/context must support a divergent image/database origin
    before such a result may qualify. All-v1 comparison leaves live=None.
    An explicit tag selects an exact historical image; it grants no rollback.
    """
    return _resolve(client, repo_dir, git_binary, selected_tag, candidate_sha, forward=True)


def _contract_now():
    return datetime.now(timezone.utc)


def resolve_admission_forward(client: transport.ReleaseGh, repo_dir: Path,
                              git_binary: Path, candidate_sha: str) -> ForwardAdmission:
    """Observe the current-main owner gate inside the original source budget.

    Only the latest release may be selected. Generic preview/rollback remain
    independent; a label never repairs unsupported semantic evidence.
    """
    return _resolve(client, repo_dir, git_binary, None, candidate_sha,
                    forward=True, admission=True)


def resolve_rollback(client: transport.ReleaseGh, repo_dir: Path,
                     git_binary: Path, selected_tag: str) -> RollbackResolution:
    """Authenticate an exact target and evaluate its rollback policy.

    Both legacy source guard and mixed-chain rules run inside the original
    source operation, before final Git metadata and complete inventory checks.
    A multi-span proof is still unavailable and never replaced with normal
    forward evidence. Live provider state is deliberately not inferred here.
    """
    return _resolve(client, repo_dir, git_binary, selected_tag, None,
                    forward=False, rollback=True)


def resolve_rollback_proof(client: transport.ReleaseGh, repo_dir: Path,
                           git_binary: Path, producer_sha: str,
                           target_tag: str) -> RollbackProofResolution:
    """Prepare a current-main/current-v0 dormant span without authorizing it.

    The exact source operation retains the complete authenticated chain and
    both signed endpoint image identities. No normal ForwardResolution or
    favorable compatibility flag can replace this operation's inputs.
    """
    return _resolve(client, repo_dir, git_binary, target_tag, producer_sha,
                    forward=False, rollback_proof=True)


def _rollback_proof_source(reader, repo_dir, chain, latest, selected, producer):
    requirements = policy._rollback_requirements(chain, selected.release)
    if requirements.non_none_count < 2:
        raise policy.PolicyRefusal("rollback-proof-not-required")
    span = (requirements.target, *requirements.intermediates)
    if (producer.mode != "reviewed-v0" or not requirements.runtime_source_supported
            or any(entry.facts.db_change != "none" and
                   (entry.facts.db_change not in {"data", "expand"} or not entry.facts.dormant)
                   for entry in requirements.intermediates)):
        raise policy.PolicyRefusal("rollback-proof-source-unsupported")
    # Shape alone is insufficient: this first lane also requires identical
    # source-bound registry and seed bytes. Future transitions stay closed.
    sources = {producer.sha: producer}
    for entry in span:
        sources[entry.reader.sha] = entry.reader
        if entry.live is not None:
            origin = entry.live.origin_source
            if (origin.mode != "reviewed-v0" or entry.live.catalog != producer.catalog
                    or origin.catalog != entry.live.catalog or origin.artifacts != producer.artifacts):
                raise policy.PolicyRefusal("rollback-proof-source-unsupported")
            sources[origin.sha] = origin
    origin_sha = requirements.live.origin_source.sha
    for base in sorted({latest.record_sha, origin_sha}):
        if base != producer.sha and reader.output(repo_dir, ["merge-base", base, producer.sha],
                code="git-failed:source-ancestry") != (base + "\n").encode("ascii"):
            raise policy.PolicyRefusal("rollback-proof-source-unsupported")
    endpoints = {selected.actual_web_source_sha, latest.actual_web_source_sha}
    expected = dict(producer.artifacts)
    materials = []
    for sha, facts in sorted(sources.items()):
        legacy = (facts.mode == "legacy-reader0" and sha == policy.V0_RELEASED_SOURCE
                  and sha in endpoints)
        if facts.mode not in {"reviewed-v0", "legacy-reader0"}:
            raise policy.PolicyRefusal("rollback-proof-source-unsupported")
        if facts.mode == "reviewed-v0" and (facts.catalog != producer.catalog
                                            or facts.artifacts != producer.artifacts):
            raise policy.PolicyRefusal("rollback-proof-source-unsupported")
        observations, total = [], 0
        for path in policy.SOURCE_PATHS:
            raw = reader.read_blob(sha, path)
            if legacy and path in {policy.PROFILE, policy.OVERLAY}:
                valid = raw is None
            else:
                valid = type(raw) is bytes and bool(raw) and common.sha256_bytes(raw) == expected[path]
            if not valid:
                raise policy.PolicyRefusal("rollback-proof-source-unsupported")
            total += len(raw) if raw is not None else 0
            if total > policy.MAX_SOURCE_BYTES:
                raise policy.PolicyRefusal("record-source-invalid")
            observations.append(BlobObservation(path, raw))
        materials.append(RollbackSourceMaterial(sha, "legacy-e001-absent-overlay" if legacy
                         else "equal-reviewed-v0", tuple(observations)))
    workflow = reader.read_blob(producer.sha, ".github/workflows/compat.yml")
    if not workflow:
        raise policy.PolicyRefusal("record-source-invalid")
    target_recipe = _web_recipe(reader, selected)
    live_recipe = target_recipe if latest.actual_web_source_sha == selected.actual_web_source_sha else _web_recipe(reader, latest)
    return requirements, workflow, tuple(materials), target_recipe, live_recipe


def _rollback_source(chain, repo_dir, selected_tag):
    if chain.entries[-1].live is not None:
        return None, None, policy.rollback_plan(chain, selected_tag)
    # Keep the pre-v2 emergency route available without inventing a measured
    # live tuple. These are the original canonical v1 bytes, not v2 coercions.
    if any(entry.facts.schema_version != 1 for entry in chain.entries):
        raise policy.PolicyRefusal("record-policy-legacy-live-unavailable")
    documents = tuple(common.loads_strict(item.raw) for item in chain.inputs)
    entries = (records.bootstrap_entry(documents[0], chain.inputs[0].manifest_sha256),
               *(records.manifest_entry(document, item.raw)
                 for document, item in zip(documents[1:], chain.inputs[1:], strict=True)))
    legacy = records.Chain(entries)
    target, _intermediates = records.rollback_plan(legacy, selected_tag)
    classification = schema_change.guard(repo_dir, target.sha, legacy.latest.sha, mode="rollback")
    return legacy, classification, None


def _catalog_material(reader, latest, selected, candidate):
    def observations(sha, absent=()):
        result = []
        for path in _CATALOG_PATHS:
            raw = reader.read_blob(sha, path)
            if path in absent:
                valid = raw is None
            else:
                valid = type(raw) is bytes and common.sha256_bytes(raw) == _CATALOG_PINS[path]
            if not valid:
                raise policy.PolicyRefusal("record-source-unsupported")
            result.append(BlobObservation(path, raw))
        return tuple(result)

    candidate_observations = observations(candidate.sha)
    candidate_hashes = dict(candidate.artifacts)
    if any(candidate_hashes.get(row.path) != common.sha256_bytes(row.raw) for row in candidate_observations):
        raise policy.PolicyRefusal("record-source-invalid")
    candidate_blobs = tuple(policy.SourceBlob(row.path, row.raw) for row in candidate_observations)
    if latest.live is not None:
        origin = latest.live.origin_source
        if origin.mode != "reviewed-v0" or latest.live.catalog != origin.catalog:
            raise policy.PolicyRefusal("record-source-unsupported")
        baseline_sha, provenance = origin.sha, "retained-live-v2"
        baseline = observations(baseline_sha)
        origin_hashes = dict(origin.artifacts)
        if any(origin_hashes.get(row.path) != common.sha256_bytes(row.raw) for row in baseline):
            raise policy.PolicyRefusal("record-source-invalid")
    else:
        # Source comparison and selected image catalog fallback are distinct.
        # This proves fixed source observations, never a legacy live contract.
        baseline_sha = selected.actual_web_source_sha
        if baseline_sha == policy.V0_RELEASED_SOURCE:
            provenance = "legacy-e001-absent-overlay"
            baseline = observations(baseline_sha, (policy.PROFILE, policy.OVERLAY))
        else:
            provenance = "legacy-equal-reviewed-v0"
            baseline = observations(baseline_sha)
    return CatalogMaterial(candidate.sha, candidate_blobs, baseline_sha, provenance, baseline)


def _web_recipe(reader, selected):
    raw = reader.read_blob(selected.actual_web_source_sha, _WEB_RECIPE)
    if not raw:
        raise policy.PolicyRefusal("record-source-invalid")
    return WebRecipe(selected.actual_web_source_sha, _WEB_RECIPE, raw, common.sha256_bytes(raw))


def _forward_source(reader, repo_dir, latest, candidate):
    candidate_sha = candidate.sha
    database_base = latest.live.origin_source.sha if latest.live is not None else latest.record_sha
    for base in sorted({latest.record_sha, database_base}):
        if base != candidate_sha and reader.output(repo_dir, ["merge-base", base, candidate_sha],
                code="git-failed:source-ancestry") != (base + "\n").encode("ascii"):
            raise policy.PolicyRefusal("record-policy-invalid")
    classification = schema_change.classify_v2(repo_dir, database_base, candidate_sha)
    workflow = reader.read_blob(candidate_sha, ".github/workflows/compat.yml")
    if not workflow:
        raise policy.PolicyRefusal("record-source-invalid")
    return candidate, classification, workflow, database_base


def _audit_roles(database_base, latest_sha, web_sha):
    grouped = {}
    for role, sha in (("database-origin", database_base), ("latest-record", latest_sha),
                      ("selected-web", web_sha)):
        if type(sha) is not str or common.SHA1_RE.fullmatch(sha) is None:
            raise policy.PolicyRefusal("record-source-invalid")
        grouped.setdefault(sha, []).append(role)
    return tuple((sha, tuple(roles)) for sha, roles in sorted(grouped.items()))


def validate_forward_audit(audit, *, candidate_sha, database_base_source,
                           latest_record_sha, selected_web_source_sha):
    """Check a complete immutable projection; this cannot authenticate bytes.

    The diff cap covers the complete canonical range projection. Drill bytes
    retain the existing source-file cap and are not interpreted here: malformed
    irrelevant drill metadata must not accidentally become a required gate.
    """
    def need(ok):
        if not ok:
            raise policy.PolicyRefusal("record-source-invalid")

    need(type(candidate_sha) is str and common.SHA1_RE.fullmatch(candidate_sha) is not None)
    need(type(audit) is ForwardAuditMaterial and audit.candidate_sha == candidate_sha)
    need(type(audit.drills) is BlobObservation and audit.drills.path == _DRILLS
         and (audit.drills.raw is None or (type(audit.drills.raw) is bytes
              and len(audit.drills.raw) <= policy.MAX_SOURCE_FILE_BYTES)))
    expected = _audit_roles(database_base_source, latest_record_sha, selected_web_source_sha)
    need(type(audit.changes) is tuple and len(audit.changes) == len(expected))
    projection = []
    for row, (base, roles) in zip(audit.changes, expected, strict=True):
        need(type(row) is ChangedRange and type(row.roles) is tuple and row.roles == roles
             and row.base_sha == base and row.head_sha == candidate_sha
             and type(row.rows) is tuple and len(row.rows) <= MAX_CHANGED_PATHS)
        paths = []
        for item in row.rows:
            need(type(item) is ChangedPath and type(item.status) is str
                 and item.status in ("A", "D", "M", "T", "U", "X")
                 and source_git._path(item.path))
            paths.append(item.path)
        need(paths == sorted(set(paths)))
        need(base != candidate_sha or not row.rows)
        projection.append({"roles": roles, "base_sha": base, "head_sha": candidate_sha,
                           "rows": [(item.status, item.path) for item in row.rows]})
    need(len(common.canonical_file_bytes(projection)) <= MAX_AUDIT_BYTES)
    return audit


def _forward_audit(reader, repo_dir, latest, selected, candidate_sha, database_base):
    drills = BlobObservation(_DRILLS, reader.read_blob(candidate_sha, _DRILLS))
    changes = []
    for base, roles in _audit_roles(database_base, latest.record_sha, selected.actual_web_source_sha):
        raw = reader.output(repo_dir, ["diff", "--no-renames", "--name-status", "-z",
                            base, candidate_sha, "--"], code="git-failed:source-audit")
        if type(raw) is not bytes or (raw and not raw.endswith(b"\0")):
            raise policy.PolicyRefusal("record-source-invalid")
        fields = raw.split(b"\0")[:-1] if raw else []
        if len(fields) % 2 or len(fields) > MAX_CHANGED_PATHS * 2:
            raise policy.PolicyRefusal("record-source-invalid")
        rows = tuple(ChangedPath(fields[i].decode("ascii"), fields[i + 1].decode("utf-8"))
                     for i in range(0, len(fields), 2))
        changes.append(ChangedRange(roles, base, candidate_sha,
                                    tuple(sorted(rows, key=lambda item: item.path))))
    return validate_forward_audit(ForwardAuditMaterial(candidate_sha, drills, tuple(changes)),
        candidate_sha=candidate_sha, database_base_source=database_base,
        latest_record_sha=latest.record_sha, selected_web_source_sha=selected.actual_web_source_sha)


def _forward_pending(reader, repo_dir, latest_sha, candidate_sha, database_base, material, classification):
    """Retain complete observations; malformed plans stay blocked previews.

    Endpoint capture/framing/mode/bounds failures are hard resolver refusals.
    Complete observed invalid plans and intermediate history remain explicitly
    blocked previews; no raw endpoint bytes are omitted or replaced by absence.
    """
    active = {row.path: row.raw for row in material.candidate_blobs}
    active_args = dict(active_catalog=active[policy.GOLDEN], active_globals=active[policy.GLOBALS],
                       active_history=active[policy.HISTORY])
    inventories = {}
    for sha in sorted({candidate_sha, latest_sha}):
        raw = reader.output(repo_dir, ["ls-tree", "-r", "--name-only", "-z", sha, "--",
                            pending_source.plan.PREFIX.rstrip("/")], code="git-failed:source-pending")
        if type(raw) is not bytes or len(raw) > pending_source.MAX_FILE_BYTES or (raw and not raw.endswith(b"\0")):
            raise policy.PolicyRefusal("record-source-invalid")
        names = raw[:-1].decode("utf-8").split("\0") if raw else []
        if (len(names) > 3 or names != sorted(set(names))
                or any(pending_source._FILE.fullmatch(name) is None for name in names)):
            raise policy.PolicyRefusal("record-source-invalid")
        files = []
        for path in names:
            body = reader.read_blob(sha, path)
            if type(body) is not bytes or not 0 < len(body) <= pending_source.MAX_FILE_BYTES:
                raise policy.PolicyRefusal("record-source-invalid")
            files.append(pending_source.PendingBlob(path, body))
        inventories[sha] = tuple(files)
    pending = pending_source.PendingMaterial(candidate_sha, latest_sha,
        inventories[candidate_sha], inventories[latest_sha])
    if (sum(len(raw) for raw in active_args.values())
            + sum(len(row.raw) for rows in (pending.candidate_files, pending.previous_files) for row in rows)
            > pending_source.MAX_MATERIAL_BYTES):
        raise policy.PolicyRefusal("record-source-invalid")
    reasons, requirements = set(classification.reasons), set(classification.requirements)
    summary = [line for line in classification.summary if not line.startswith("policy classification:")]
    kind, dormant = classification.kind, classification.dormant
    if latest_sha != database_base:
        # Rollback can separate latest source from database origin. Endpoint
        # hashes cannot reveal a transient plan removal/replacement on this
        # second range. Reuse only the bounded pending traversal, not a second
        # complete classifier. Its reads share the original Git/outer budgets.
        range_reasons, range_requirements, range_summary = set(), set(), []
        try:
            introduced = schema_change._v2_pending(schema_change._Repository(repo_dir), latest_sha,
                candidate_sha, range_reasons, range_summary, range_requirements)
            if introduced and kind == "none":
                kind, dormant = "expand", True
        except (schema_change._Invalid, UnicodeError, TypeError, ValueError, KeyError, RecursionError):
            reasons.add("classifier-v2-invalid:pending-latest-record")
            requirements.update(("pending-format-unsupported", "producer-class-unsupported"))
        reasons.update(range_reasons)
        requirements.update(range_requirements)
        summary.extend(line for line in range_summary if line not in summary)
    try:
        projection = pending_source.replay(pending, candidate_sha=candidate_sha, previous_sha=latest_sha, **active_args)
        if projection.plan is not None and any(entry.kind != "expand" for entry in projection.plan.entries):
            reasons.add("pending-format-unsupported")
            requirements.update(("pending-format-unsupported", "producer-class-unsupported"))
    except pending_source.PendingSourceRefusal:
        reasons.update(("pending-format-unsupported", "classifier-v2-invalid:pending-material"))
        requirements.update(("pending-format-unsupported", "producer-class-unsupported"))
    summary.append("policy classification: " + kind + (" (dormant)" if dormant else ""))
    if len(summary) > 25000:
        raise policy.PolicyRefusal("record-source-invalid")
    for line in (*reasons, *summary):
        common.require_public_text(line)
    classification = schema_change.PolicyClassification(kind, dormant, tuple(sorted(reasons)), tuple(summary),
        classification.migration_changes, tuple(sorted(requirements)))
    return pending, classification


def _resolve(client, repo_dir, git_binary, selected_tag, candidate_sha, *, forward, rollback=False,
             admission=False, rollback_proof=False):
    if type(client) is not transport.ReleaseGh or type(client.budget) is not transport.OperationBudget:
        raise transport.TransportRefusal("record-chain-invalid:resolver-client")
    budget = client.budget
    try:
        if admission and (not forward or rollback or selected_tag is not None):
            budget.fail("resolver-selection")
        if rollback and (forward or selected_tag is None):
            budget.fail("resolver-selection")
        if rollback_proof and (forward or rollback or admission or selected_tag is None):
            budget.fail("resolver-selection")
        if (forward or rollback_proof) and (type(candidate_sha) is not str or common.SHA1_RE.fullmatch(candidate_sha) is None):
            budget.fail("resolver-candidate")
        if selected_tag is not None and (type(selected_tag) is not str or
                (selected_tag != common.BOOTSTRAP_TAG and common.RECORD_TAG_RE.fullmatch(selected_tag) is None)):
            budget.fail("resolver-selection")
        snapshot = client.capture_snapshot()
        if (admission or rollback_proof) and candidate_sha != snapshot.main_sha:
            budget.fail("resolver-main")
        bootstrap = records.load_bootstrap()
        bootstrap_raw = common.canonical_file_bytes(bootstrap)
        # Consume the entire inventory before deriving policy. Each yielded
        # value has already passed the existing canonical/signature checks.
        raw_records = tuple(records.collect_authenticated_records(client, work_dir=client.work_dir))
        documents = (bootstrap, *(records.validate_manifest(common.loads_strict(row.raw)) for row in raw_records))
        images = (client.verify_selected_images(None),
                  *(client.verify_selected_images(row) for row in raw_records))
        for document, binding in zip(documents, images, strict=True):
            expected_images = tuple((key, document["images"][key]) for key in common.COMPONENTS)
            expected_sources = tuple((key, bootstrap["legacy_signer"]["source_commit"]
                if document["images"][key] == bootstrap["images"][key] else document["sha"])
                for key in common.COMPONENTS)
            if (type(binding) is not transport.VerifiedImages or binding.release != document["release"]
                    or binding.record_sha != document["sha"] or binding.images != expected_images
                    or binding.source_shas != expected_sources):
                budget.fail("resolver-images")
        legacy, required, readers, commits = _source_requirements(documents, images)
        if forward or rollback_proof:
            # Plan this before any facts are decoded: the candidate may be a
            # SHA already seen in the historical v1 prefix. Its complete
            # contract must win there too, with one consistent SourceFacts.
            required.add(candidate_sha)
            commits.add(candidate_sha)
        reader = source_git.ImmutableGit(repo_dir, git_binary,
            limits=source_git.Limits(seconds=budget.remaining()), capture=budget.capture, clock=budget.clock)
        with reader.operation():
            for sha in sorted(commits):
                if reader.output(repo_dir, ["cat-file", "-t", sha], code="git-failed:source-commit") != b"commit\n":
                    budget.fail("resolver-commit")
            facts = []
            for sha in sorted(readers | required):
                if sha in required:
                    # A missing/unsupported complete contract cannot fall back
                    # to reader0, even if this SHA also appears in the v1 prefix.
                    facts.append(reader.reviewed_v0(sha))
                elif sha in legacy:
                    facts.append(policy.legacy_reader0(sha))
                else:
                    budget.fail("resolver-source")
            ancestors = []
            for prior, current in zip(documents, documents[1:]):
                pair = prior["sha"], current["sha"]
                if current["kind"] == "promote" and pair[0] != pair[1] and pair not in ancestors:
                    if reader.output(repo_dir, ["merge-base", *pair], code="git-failed:source-ancestry") != (pair[0] + "\n").encode("ascii"):
                        budget.fail("resolver-ancestry")
                    ancestors.append(pair)
            raw = (bootstrap_raw, *(row.raw for row in raw_records))
            inputs = tuple(policy.RecordInput(data, common.sha256_bytes(data), dict(binding.source_shas)["web"])
                           for data, binding in zip(raw, images, strict=True))
            chain = policy.normalize_chain(inputs, tuple(facts), tuple(ancestors))
            if not chain.runtime_source_supported or chain.baseline is not None:
                budget.fail("resolver-source")
            latest = _selection(chain, len(chain.entries) - 1)
            tag = latest.release if selected_tag is None else selected_tag
            matches = [index for index, entry in enumerate(chain.entries) if entry.release == tag]
            if len(matches) != 1:
                budget.fail("resolver-selection")
            selected = _selection(chain, matches[0])
            if forward:
                candidate = next(fact for fact in facts if fact.sha == candidate_sha)
                candidate, classification, workflow, database_base = _forward_source(
                    reader, repo_dir, latest, candidate)
                material = _catalog_material(reader, latest, selected, candidate)
                recipe = _web_recipe(reader, selected)
                audit = _forward_audit(reader, repo_dir, latest, selected, candidate_sha, database_base)
                pending, classification = _forward_pending(reader, repo_dir, latest.record_sha, candidate_sha,
                    database_base, material, classification)
                observation = None
                if admission and classification.kind == "contract":
                    if classification.dormant is not False:
                        raise contract.ContractRefusal("contract-label-classification")
                    observation = contract_collect.collect_contract(client, candidate_sha,
                        wall_clock=_contract_now)
                    if (type(observation) is not contract.ContractObservation
                            or type(observation.observed_at) is not datetime
                            or observation.observed_at.tzinfo is not timezone.utc
                            or observation.observed_at.fold != 0):
                        raise contract.ContractRefusal("contract-label-observation-time")
                    contract.require_contract_label(observation, candidate_sha=candidate_sha,
                        repository_id=transport.REPOSITORY_ID, now=observation.observed_at)
            if rollback:
                legacy_chain, legacy_classification, decision = _rollback_source(chain, repo_dir, tag)
            if rollback_proof:
                producer = next(fact for fact in facts if fact.sha == candidate_sha)
                requirements, workflow, materials, target_recipe, live_recipe = _rollback_proof_source(
                    reader, repo_dir, chain, latest, selected, producer)
            budget.check()
        # Git's final metadata preflight happens before the final complete
        # GitHub snapshot. Neither creates a new allowance or resets the clock.
        client.recheck(snapshot, selected_tag=selected.release, latest_tag=latest.release)
        budget.check()
        resolved = ResolvedChain(chain, snapshot, images, latest, selected)
        if forward:
            result = ForwardResolution(resolved, candidate, classification, workflow,
                common.sha256_bytes(workflow), selected.actual_web_source_sha, database_base,
                material, recipe, audit, pending)
            return ForwardAdmission(result, observation) if admission else result
        if rollback:
            return RollbackResolution(resolved, legacy_chain, legacy_classification, decision)
        if rollback_proof:
            return RollbackProofResolution(resolved, requirements, producer, workflow,
                common.sha256_bytes(workflow), materials, target_recipe, live_recipe)
        return resolved
    except contract.ContractRefusal:
        budget.failed = True
        raise
    except (policy.PolicyRefusal, schema_change.SchemaChangeBlocked):
        if rollback or rollback_proof:
            # These two reviewed policy classes carry fixed refusal codes or
            # structured classifier reasons, never arbitrary driver output.
            budget.failed = True
            raise
        budget.fail("resolver")
    except Exception:
        # Neither source/CLI errors nor arbitrary driver messages are public.
        budget.fail("resolver")
    finally:
        budget.close()
