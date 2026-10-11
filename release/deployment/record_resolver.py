"""Preparatory authenticated record/source resolution; no deployment authority.

The concrete transport owns signature verification. The immutable projections
below retain that operation's inputs, not a transferable cryptographic receipt.
Selection does not invoke rollback policy or fabricate a legacy live catalog.
Only the reviewed v0 source format and authenticated v1-prefix reader0 exist.
"""
from dataclasses import dataclass
from pathlib import Path

import record_policy as policy
import record_transport as transport
import release_record as records
import schema_change
import ship_common as common
import source_git

_CATALOG_PINS = dict(policy.V0_PINS)
_CATALOG_PATHS = tuple(sorted(_CATALOG_PINS))
_WEB_RECIPE = "docker/release/web.Dockerfile"


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


def _resolve(client, repo_dir, git_binary, selected_tag, candidate_sha, *, forward):
    if type(client) is not transport.ReleaseGh or type(client.budget) is not transport.OperationBudget:
        raise transport.TransportRefusal("record-chain-invalid:resolver-client")
    budget = client.budget
    try:
        if forward and (type(candidate_sha) is not str or common.SHA1_RE.fullmatch(candidate_sha) is None):
            budget.fail("resolver-candidate")
        if selected_tag is not None and (type(selected_tag) is not str or
                (selected_tag != common.BOOTSTRAP_TAG and common.RECORD_TAG_RE.fullmatch(selected_tag) is None)):
            budget.fail("resolver-selection")
        snapshot = client.capture_snapshot()
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
        if forward:
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
            budget.check()
        # Git's final metadata preflight happens before the final complete
        # GitHub snapshot. Neither creates a new allowance or resets the clock.
        client.recheck(snapshot, selected_tag=selected.release, latest_tag=latest.release)
        budget.check()
        resolved = ResolvedChain(chain, snapshot, images, latest, selected)
        if forward:
            return ForwardResolution(resolved, candidate, classification, workflow,
                common.sha256_bytes(workflow), selected.actual_web_source_sha, database_base,
                material, recipe)
        return resolved
    except Exception:
        # Neither source/CLI errors nor arbitrary driver messages are public.
        budget.fail("resolver")
    finally:
        budget.close()
