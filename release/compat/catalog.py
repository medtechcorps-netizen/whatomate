"""Source-bound reviewed-v0 catalog observations, not a catalog normalizer.

The caller first verifies the previous signed release using runtime.verified_previous.
This module then reads fixed paths from its immutable Git commit and candidate
commit; working-tree files cannot supply the expected table inventory. The
initial e001 baseline has no overlay. Later verified releases must contain the
same pinned v0 artifacts as the candidate, including its physical-order profile
and comments-only NOP overlay. Other catalog progression refuses rather than
applying arbitrary head DDL to an old release.

Catalog SHA values come only from the actual owned candidate Go CLI. The equal
embedded golden contract lets that CLI observe every current phase against the
same identities. No owner credentials, unknown names, seed values or raw logs
are public outputs. Docker/Go/DB operations never execute at import.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import hashlib
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import postgres
import processes
import record_resolver
import pending_source
import public_evidence

runtime, proof = processes.runtime, processes.proof
_BASE = "e0016b3595114728e7864da2adb44c29065f25ef"
_GOLDEN = "internal/dbcatalog/golden/catalog.json"
_PROFILE = "internal/dbcatalog/golden/production-v0.json"
_OVERLAY = "internal/dbcatalog/shape/production-v0.sql"
_PINS = {
    _GOLDEN: "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8",
    "internal/dbcatalog/golden/global_tables.json": "c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3",
    "internal/dbcatalog/golden/history.json": "9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513",
    _PROFILE: "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d",
    _OVERLAY: "48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d",
}
PENDING_ACTIVE_ARTIFACTS = public_evidence.PENDING_ACTIVE_ARTIFACTS
_COUNT_KEYS = {"non_public_schemas", "non_public_objects", "other_roles", "default_acls", "oids", "reloptions",
               "statistics", "tablespaces", "other_acl_entries", "other_policy_roles"}
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_NULLABLE_CATALOG_SHA256 = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"
# Only the CLI's fixed public diagnostics may refine a refusal. Never echo
# arbitrary process output: it may contain configuration or driver details.
_CLI_FAILURES = {
    (2, b"catalog-snapshot:invalid-arguments; use -help\n"): "catalog-cli-invalid-arguments",
    (1, b"catalog-snapshot:configuration-unavailable\n"): "catalog-cli-configuration-unavailable",
    (1, b"catalog-snapshot:capture-failed\n"): "catalog-cli-capture-failed",
    (1, b"catalog-snapshot:output-failed\n"): "catalog-cli-output-failed",
}


def need(value, code="catalog-contract-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def _json(raw):
    need(type(raw) is bytes and 0 < len(raw) <= 4*runtime.MIB)
    try:
        return runtime.common.loads_strict(raw)
    except runtime.common.ReleaseError:
        raise proof.EvidenceError("catalog-json-invalid") from None


def _nullable_expected(golden_raw, profile_raw):
    """Reconstruct only the pinned 72 deltas, without an observed input."""
    import catalog_semantics as wire
    need(type(golden_raw) is bytes and type(profile_raw) is bytes
         and hashlib.sha256(golden_raw).hexdigest() == _PINS[_GOLDEN]
         and hashlib.sha256(profile_raw).hexdigest() == _PINS[_PROFILE],
         "catalog-baseline-source-invalid")
    golden, profile = _json(golden_raw), _json(profile_raw)
    need(type(golden) is dict and set(golden) == {"version", "objects"}
         and type(golden["version"]) is int and golden["version"] == 0)
    need(type(profile) is dict and set(profile) == {"version", "live_sha256", "golden_sha256",
         "released_source", "delta", "accepted_unpublished", "n_other_schemas", "bootstrap_column_order"}
         and type(profile["version"]) is int and profile["version"] == 0
         and profile["released_source"] == _BASE and profile["golden_sha256"] == _PINS[_GOLDEN]
         and profile["live_sha256"] == _NULLABLE_CATALOG_SHA256 and profile["accepted_unpublished"] == []
         and type(profile["delta"]) is list and len(profile["delta"]) == 72,
         "catalog-baseline-source-invalid")
    wire.catalog_format._catalog(golden, 0)
    objects = golden["objects"]
    need(wire._catalog_bytes(objects) == golden_raw, "catalog-baseline-source-invalid")
    rows = {(row["kind"], row["identity"]): row for row in objects}
    inventory = tuple((row["kind"], row["identity"], row.get("parent")) for row in objects)
    seen = set()
    for delta in profile["delta"]:
        need(type(delta) is dict and set(delta) == {"kind", "identity", "disposition", "attributes"}
             and delta["disposition"] == "fix-bootstrap", "catalog-baseline-source-invalid")
        key = (delta["kind"], delta["identity"])
        need(key in rows and key not in seen and delta["kind"] in {"columns", "types"},
             "catalog-baseline-source-invalid")
        seen.add(key)
        changes = delta["attributes"]
        attribute = "position" if delta["kind"] == "columns" else "definition_sha256"
        need(type(changes) is dict and set(changes) == {attribute}, "catalog-baseline-source-invalid")
        pair = changes[attribute]
        need(type(pair) is dict and set(pair) == {"golden", "production"}
             and type(pair["golden"]) is str and type(pair["production"]) is str
             and rows[key]["attributes"][attribute] == pair["golden"]
             and pair["golden"] != pair["production"], "catalog-baseline-source-invalid")
        rows[key]["attributes"][attribute] = pair["production"]
    need(tuple((row["kind"], row["identity"], row.get("parent")) for row in objects) == inventory,
         "catalog-baseline-source-invalid")
    wire.catalog_format._catalog(golden, 0)
    raw = wire._catalog_bytes(objects)
    need(hashlib.sha256(raw).hexdigest() == _NULLABLE_CATALOG_SHA256,
         "catalog-baseline-source-invalid")
    return raw


class CatalogContract:
    def __init__(self, repo: Path, previous: runtime.release_record.Entry, candidate_sha: str,
                 *, base_env=None, capture=runtime.bounded_command):
        need(isinstance(repo, Path) and repo.is_dir() and type(previous) is runtime.release_record.Entry)
        need(type(previous.sha) is str and _SHA.fullmatch(previous.sha)
             and type(candidate_sha) is str and _SHA.fullmatch(candidate_sha),
             "catalog-progression-unsupported")
        self.previous_sha, self.candidate_sha = previous.sha, candidate_sha
        digest = previous.images.get("web")
        need(type(digest) is str and runtime.common.DIGEST_RE.fullmatch(digest))
        self.previous_web = runtime.common.IMAGE_REPOSITORY["web"] + "@" + digest
        env = runtime.common.scrubbed_env({"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"}, base=base_env)
        def git(args, limit=4*runtime.MIB):
            try:
                result = capture(["git", "-C", str(repo), *args], env=dict(env), timeout=30,
                                 stdout_limit=limit, stderr_limit=runtime.MIB)
            except Exception:
                raise proof.InfrastructureFailure("catalog-source") from None
            need(type(result) is runtime.Capture and type(result.returncode) is int and result.returncode == 0
                 and type(result.stdout) is bytes and len(result.stdout) <= limit and not result.stderr,
                 "catalog-source-invalid")
            return result.stdout
        for sha in (self.previous_sha, candidate_sha):
            need(git(["cat-file", "-t", sha],1024) == b"commit\n", "catalog-source-invalid")
        git(["merge-base", "--is-ancestor", self.previous_sha, candidate_sha],1024)
        blobs = {}
        for name in _PINS:
            raw = git(["cat-file", "blob", candidate_sha+":"+name])
            need(hashlib.sha256(raw).hexdigest() == _PINS[name], "catalog-progression-unsupported")
            blobs[name] = raw
            if previous.sha != _BASE or name not in {_PROFILE,_OVERLAY}:
                need(git(["cat-file", "blob", self.previous_sha+":"+name]) == raw, "catalog-fallback-not-equal")
            else:
                # Absence is admitted only for the fixed first baseline; no
                # generic missing-overlay fallback is inferred for later refs.
                need(git(["ls-tree", "-z", self.previous_sha, "--", name],1024) == b"", "catalog-base-overlay-unsupported")
        self._base_overlay = ("explicit-empty-first-baseline" if previous.sha == _BASE
                              else "equal-reviewed-v0-nop")
        self._load_pinned(blobs)

    @classmethod
    def from_resolution(cls, resolution: record_resolver.ForwardResolution):
        """Consume already gathered source bytes without opening a new budget.

        This is a pure consistency check, not authentication or permission to
        deploy. The future operational caller must create its own resolution
        through the concrete resolver. The previous image source remains
        separate from the retained database origin after an image rollback.
        The legacy constructor and live callers are unchanged by this method.
        """
        need(cls is CatalogContract and type(resolution) is record_resolver.ForwardResolution)
        policy = record_resolver.policy
        resolved, candidate, material = resolution.resolved, resolution.candidate, resolution.catalog_material
        need(type(resolved) is record_resolver.ResolvedChain
             and type(candidate) is policy.SourceFacts
             and type(material) is record_resolver.CatalogMaterial)
        latest, selected = resolved.latest, resolved.selected
        need(type(latest) is record_resolver.ReleaseSelection
             and type(selected) is record_resolver.ReleaseSelection)
        need(all(type(sha) is str and _SHA.fullmatch(sha) for sha in
                 (candidate.sha, latest.record_sha, selected.actual_web_source_sha, material.baseline_sha)))
        need(resolution.previous_web_source_sha == selected.actual_web_source_sha
             and material.candidate_sha == candidate.sha, "catalog-image-source-mismatch")
        try:
            policy._source(candidate)
        except policy.PolicyRefusal:
            raise proof.EvidenceError("catalog-contract-invalid") from None
        need(candidate.mode == "reviewed-v0", "catalog-progression-unsupported")
        paths = tuple(sorted(_PINS))
        need(type(material.candidate_blobs) is tuple and len(material.candidate_blobs) == len(paths)
             and all(type(row) is policy.SourceBlob for row in material.candidate_blobs)
             and tuple(row.path for row in material.candidate_blobs) == paths)
        need(type(material.baseline_blobs) is tuple and len(material.baseline_blobs) == len(paths)
             and all(type(row) is record_resolver.BlobObservation for row in material.baseline_blobs)
             and tuple(row.path for row in material.baseline_blobs) == paths)
        blobs = {row.path: row.raw for row in material.candidate_blobs}
        need(all(type(raw) is bytes and hashlib.sha256(raw).hexdigest() == _PINS[path]
                 for path, raw in blobs.items()), "catalog-progression-unsupported")
        need(all(dict(candidate.artifacts)[path] == _PINS[path] for path in paths))
        absent = set()
        if latest.live is not None:
            live = latest.live
            need(type(live) is policy.LiveContract and type(live.origin_source) is policy.SourceFacts)
            try:
                policy._source(live.origin_source)
            except policy.PolicyRefusal:
                raise proof.EvidenceError("catalog-contract-invalid") from None
            need(live.origin_source.mode == "reviewed-v0" and live.catalog == live.origin_source.catalog
                 and material.baseline_sha == live.origin_source.sha
                 and resolution.database_base_source == live.origin_source.sha
                 and material.baseline_provenance == "retained-live-v2", "catalog-fallback-not-equal")
        else:
            need(material.baseline_sha == selected.actual_web_source_sha
                 and resolution.database_base_source == latest.record_sha, "catalog-fallback-not-equal")
            if material.baseline_sha == _BASE:
                need(material.baseline_provenance == "legacy-e001-absent-overlay")
                absent = {_PROFILE, _OVERLAY}
            else:
                need(material.baseline_provenance == "legacy-equal-reviewed-v0")
        for row in material.baseline_blobs:
            need(row.raw is None if row.path in absent else type(row.raw) is bytes and row.raw == blobs[row.path],
                 "catalog-fallback-not-equal")
        need(type(selected.images) is tuple and len(selected.images) == len(runtime.common.COMPONENTS)
             and all(type(row) is tuple and len(row) == 2 for row in selected.images)
             and tuple(row[0] for row in selected.images) == runtime.common.COMPONENTS)
        digest = dict(selected.images)["web"]
        need(type(digest) is str and runtime.common.DIGEST_RE.fullmatch(digest))
        contract = cls.__new__(cls)
        contract.previous_sha, contract.candidate_sha = selected.actual_web_source_sha, candidate.sha
        contract.previous_web = runtime.common.IMAGE_REPOSITORY["web"] + "@" + digest
        contract.database_base_source = resolution.database_base_source
        contract.catalog_baseline_sha = material.baseline_sha
        contract.baseline_provenance = material.baseline_provenance
        contract._base_overlay = "explicit-empty-first-baseline" if absent else "equal-reviewed-v0-nop"
        contract._load_pinned(blobs)
        # The pending target is source metadata, never this contract's live
        # catalog, table inventory, SQL allowlist or applied-gate evidence.
        try:
            projection = pending_source.replay(resolution.pending, candidate_sha=candidate.sha,
                previous_sha=latest.record_sha, active_catalog=blobs[_GOLDEN],
                active_globals=blobs[policy.GLOBALS], active_history=blobs[policy.HISTORY])
        except pending_source.PendingSourceRefusal:
            raise proof.EvidenceError("catalog-pending-invalid") from None
        need(projection.plan is None or all(entry.kind == "expand" for entry in projection.plan.entries),
             "catalog-pending-unsupported")
        contract.pending_binding = projection.binding
        return contract

    @classmethod
    def from_rollback_resolution(cls, resolution: record_resolver.RollbackProofResolution):
        """Pure complete consistency replay of the preparatory rollback source.

        This data does not authenticate signatures, commit ancestry or freshness;
        the concrete resolver owns those observations. No additional source read
        or H image-recipe assertion is made here. T/L recipes describe only their
        actual image sources; H admission retains its independent build receipt.
        """
        need(cls is CatalogContract and type(resolution) is record_resolver.RollbackProofResolution)
        policy, transport = record_resolver.policy, record_resolver.transport
        try:
            resolved, producer = resolution.resolved, resolution.producer
            need(type(resolved) is record_resolver.ResolvedChain
                 and type(resolved.chain) is policy.PolicyChain
                 and type(producer) is policy.SourceFacts)
            chain = resolved.chain
            need(chain.baseline is None and chain.runtime_source_supported is True)
            need(policy.normalize_chain(chain.inputs, chain.sources, chain.ancestors) == chain)
            latest, selected = resolved.latest, resolved.selected
            need(type(latest) is record_resolver.ReleaseSelection
                 and type(selected) is record_resolver.ReleaseSelection
                 and type(selected.index) is int and 0 <= selected.index < len(chain.entries)
                 and type(latest.index) is int and latest.index == len(chain.entries) - 1)
            need(latest == record_resolver._selection(chain, latest.index)
                 and selected == record_resolver._selection(chain, selected.index))
            required = policy._rollback_requirements(chain, selected.release)
            need(type(resolution.requirements) is policy.RollbackRequirements
                 and resolution.requirements == required and required.non_none_count >= 2
                 and required.runtime_source_supported is True, "catalog-rollback-span-invalid")
            need(all(entry.facts.db_change == "none" or
                     (entry.facts.db_change in {"data", "expand"} and entry.facts.dormant is True)
                     for entry in required.intermediates), "catalog-progression-unsupported")
            policy._source(producer)
            need(producer.mode == "reviewed-v0" and producer in chain.sources)
            documents = tuple(runtime.common.loads_strict(row.raw) for row in chain.inputs)
            bootstrap = documents[0]  # normalize_chain verified the pinned raw bootstrap.
            images = resolved.image_bindings
            need(type(images) is tuple and len(images) == len(documents))
            for document, binding in zip(documents, images, strict=True):
                expected_images = tuple((key, document["images"][key]) for key in runtime.common.COMPONENTS)
                expected_sources = tuple((key, bootstrap["legacy_signer"]["source_commit"]
                    if document["images"][key] == bootstrap["images"][key] else document["sha"])
                    for key in runtime.common.COMPONENTS)
                need(type(binding) is transport.VerifiedImages and binding.release == document["release"]
                     and binding.record_sha == document["sha"] and binding.images == expected_images
                     and binding.source_shas == expected_sources, "catalog-image-source-mismatch")
            legacy, complete, readers, _commits = record_resolver._source_requirements(documents, images)
            complete.add(producer.sha)
            facts = {row.sha: row for row in chain.sources}
            need(tuple(row.sha for row in chain.sources) == tuple(sorted(readers | complete)))
            need(all(facts[sha].mode == "reviewed-v0" if sha in complete else
                     sha in legacy and facts[sha].mode == "legacy-reader0" for sha in facts))
            inventory = resolved.inventory
            need(type(inventory) is transport.InventorySnapshot
                 and type(inventory.repository_id) is int and inventory.repository_id == transport.REPOSITORY_ID
                 and inventory.main_sha == producer.sha)
            records = tuple(sorted(chain.entries[1:], key=lambda row: row.release))
            need(type(inventory.records) is tuple and type(inventory.tags) is tuple
                 and len(inventory.records) == len(inventory.tags) == len(records))
            for entry, row, tag in zip(records, inventory.records, inventory.tags, strict=True):
                need(type(row) is transport.ReleaseIdentity and type(tag) is transport.TagIdentity
                     and row.release == tag.release == entry.release and tag.sha == entry.sha
                     and type(row.release_id) is int and row.release_id > 0
                     and type(row.asset_id) is int and row.asset_id > 0
                     and type(row.size) is int and row.size == len(entry.record.raw)
                     and (row.digest is None or row.digest == "sha256:" + entry.record.manifest_sha256))
            need(len({row.asset_id for row in inventory.records}) == len(records)
                 and len({row.release_id for row in inventory.records}) == len(records))
            sources = {producer.sha: producer}
            for entry in (required.target, *required.intermediates):
                sources[entry.reader.sha] = entry.reader
                if entry.live is not None:
                    origin = entry.live.origin_source
                    need(origin.mode == "reviewed-v0" and entry.live.catalog == producer.catalog
                         and origin.catalog == entry.live.catalog and origin.artifacts == producer.artifacts,
                         "catalog-fallback-not-equal")
                    sources[origin.sha] = origin
            materials = resolution.materials
            need(type(materials) is tuple and len(materials) == len(sources)
                 and all(type(row) is record_resolver.RollbackSourceMaterial for row in materials)
                 and tuple(row.source_sha for row in materials) == tuple(sorted(sources)))
            endpoints = {selected.actual_web_source_sha, latest.actual_web_source_sha}
            expected = dict(producer.artifacts)
            material_map = {}
            for material in materials:
                source = sources[material.source_sha]
                absent = (source.mode == "legacy-reader0" and source.sha == policy.V0_RELEASED_SOURCE
                          and source.sha in endpoints)
                need(source.mode in {"reviewed-v0", "legacy-reader0"})
                need(material.provenance == ("legacy-e001-absent-overlay" if absent else "equal-reviewed-v0"))
                need(type(material.blobs) is tuple and len(material.blobs) == len(policy.SOURCE_PATHS)
                     and all(type(row) is record_resolver.BlobObservation for row in material.blobs)
                     and tuple(row.path for row in material.blobs) == policy.SOURCE_PATHS)
                total = 0
                for row in material.blobs:
                    if absent and row.path in {policy.PROFILE, policy.OVERLAY}:
                        need(row.raw is None, "catalog-fallback-not-equal")
                    else:
                        need(type(row.raw) is bytes and 0 < len(row.raw) <= policy.MAX_SOURCE_FILE_BYTES
                             and hashlib.sha256(row.raw).hexdigest() == expected[row.path],
                             "catalog-fallback-not-equal")
                        total += len(row.raw)
                need(total <= policy.MAX_SOURCE_BYTES)
                if not absent:
                    decoded = policy.reviewed_v0_source(source.sha,
                        tuple(policy.SourceBlob(row.path, row.raw) for row in material.blobs))
                    need(source == decoded if source.mode == "reviewed-v0" else
                         source == policy.legacy_reader0(source.sha))
                material_map[source.sha] = {row.path: row.raw for row in material.blobs}
            need(type(resolution.workflow) is bytes and 0 < len(resolution.workflow) <= policy.MAX_SOURCE_FILE_BYTES
                 and type(resolution.workflow_sha256) is str
                 and hashlib.sha256(resolution.workflow).hexdigest() == resolution.workflow_sha256)
            for selection, recipe in ((selected, resolution.target_recipe), (latest, resolution.live_recipe)):
                need(type(recipe) is record_resolver.WebRecipe and recipe.source_sha == selection.actual_web_source_sha
                     and recipe.path == "docker/release/web.Dockerfile" and type(recipe.raw) is bytes
                     and 0 < len(recipe.raw) <= policy.MAX_SOURCE_FILE_BYTES
                     and type(recipe.sha256) is str and hashlib.sha256(recipe.raw).hexdigest() == recipe.sha256,
                     "catalog-image-source-mismatch")
            need(selected.actual_web_source_sha != latest.actual_web_source_sha
                 or resolution.target_recipe == resolution.live_recipe, "catalog-image-source-mismatch")
            contract = cls.__new__(cls)
            contract.purpose = "rollback-v0"
            contract.previous_sha, contract.candidate_sha = selected.actual_web_source_sha, latest.actual_web_source_sha
            contract.previous_web = runtime.common.IMAGE_REPOSITORY["web"] + "@" + dict(selected.images)["web"]
            contract.candidate_web = runtime.common.IMAGE_REPOSITORY["web"] + "@" + dict(latest.images)["web"]
            contract.catalog_control_sha = producer.sha
            contract.database_base_source = contract.catalog_baseline_sha = required.live.origin_source.sha
            contract.baseline_provenance = "retained-live-v2"
            contract.target_recipe, contract.live_recipe = resolution.target_recipe, resolution.live_recipe
            contract._base_overlay = "equal-reviewed-v0-nop"
            contract._load_pinned(material_map[producer.sha])
            return contract
        except (policy.PolicyRefusal, runtime.common.ReleaseError, KeyError, IndexError,
                AttributeError, TypeError, ValueError, RecursionError):
            raise proof.EvidenceError("catalog-rollback-invalid") from None

    def _load_pinned(self, blobs):
        golden, profile = _json(blobs[_GOLDEN]), _json(blobs[_PROFILE])
        need(type(golden) is dict and set(golden) == {"version","objects"} and golden["version"] == 0)
        need(profile["released_source"] == _BASE and profile["golden_sha256"] == _PINS[_GOLDEN]
             and profile["accepted_unpublished"] == [] and _HASH.fullmatch(profile["live_sha256"]))
        need(all(not line.strip() or line.lstrip().startswith(b"--") for line in blobs[_OVERLAY].splitlines()))
        self.target_sha256 = profile["live_sha256"]
        self._known = frozenset((o["kind"],o["identity"]) for o in golden["objects"])
        self._relations = frozenset(o["identity"] for o in golden["objects"] if o["kind"]=="relations")
        self._kinds = frozenset(o["kind"] for o in golden["objects"])
        tables = [o for o in golden["objects"] if o["kind"]=="relations" and o["attributes"]["relkind"] in {"r","p","m","f"}]
        need(tables and all(o["attributes"]["relkind"]!="f" for o in tables))
        self.expected_preexisting_tables = tuple(sorted(postgres.identity(o["identity"]) for o in tables))
        need(len(set(self.expected_preexisting_tables)) == len(tables))
        self._seal = object()
        self._pins = dict(_PINS)
        # Private original source bytes, not data from an observation or a new
        # H-only profile attributed to the historical database origin.
        self._nullable_source = (blobs[_GOLDEN], blobs[_PROFILE])

    def parse_nullable_baseline(self, raw):
        import catalog_semantics_v2 as semantic
        try:
            expected = _nullable_expected(*self._nullable_source)
            observation = semantic.parse_observation(raw)
            value, facts, _ = semantic._observation(semantic.canonical_observation(observation))
            snapshot, graph = value["snapshot"], value["graph"]
            need(snapshot["sha256"] == self.target_sha256 == _NULLABLE_CATALOG_SHA256
                 and semantic.base._catalog_bytes(snapshot["objects"]) == expected
                 and frozenset((ref.kind, ref.identity) for ref in facts) == self._known,
                 "catalog-baseline-mismatch")
            need(graph["relation"]["object"]["identity"] == "organizations"
                 and graph["security"]["default_acl_rows"] == semantic._contract(),
                 "catalog-baseline-profile-invalid")
            return observation
        except proof.EvidenceError:
            raise
        except (semantic.SemanticRefusal, semantic.base.catalog_format.PendingRefusal,
                TypeError, ValueError, KeyError, AttributeError, RecursionError, UnicodeError):
            raise proof.EvidenceError("catalog-baseline-output-invalid") from None

    def check_initial_tables(self, tables: postgres.TableCapture):
        need(type(tables) is postgres.TableCapture and tables._all_columns
             and tables.identities() == list(self.expected_preexisting_tables), "catalog-table-inventory-mismatch")

    def parse_compare(self, raw):
        need(type(raw) is bytes and 0<len(raw)<=runtime.MIB and raw.endswith(b"\n"), "catalog-output-invalid")
        value = _json(raw)
        need(type(value) is dict and set(value)=={"sha256","match","differences","unknown","count_only","ownership"}, "catalog-output-invalid")
        need(type(value["sha256"]) is str and _HASH.fullmatch(value["sha256"])
             and type(value["match"]) is bool and type(value["differences"]) is list, "catalog-output-invalid")
        seen=set()
        for row in value["differences"]:
            need(type(row) is dict and set(row)=={"kind","identity","status"}
                 and type(row["kind"]) is str and type(row["identity"]) is str, "catalog-output-invalid")
            key=(row["kind"],row["identity"])
            need(key in self._known and key not in seen and type(row["status"]) is str and row["status"] in {"changed","missing"}, "catalog-output-invalid")
            seen.add(key)
        need(value["match"] is (not seen), "catalog-output-invalid")
        # The reviewed v0 overlay admits no unpublished identities. A known-only
        # SHA cannot hide new unknown functions, relations or other objects.
        need(value["unknown"] == [] and type(value["unknown"]) is list, "catalog-unknown-unsupported")
        counts=value["count_only"]
        need(type(counts) is dict and set(counts)==_COUNT_KEYS and all(type(v) is int and 0<=v<2**63 for v in counts.values()), "catalog-output-invalid")
        ownership=value["ownership"]
        need(type(ownership) is dict and set(ownership)=={"migration_owner_owns_database","migration_owner_owns_public_schema"}
             and all(v is True for v in ownership.values()), "catalog-ownership-invalid")
        return value["sha256"]

    def public(self):
        return {"previous_sha":self.previous_sha,"candidate_sha":self.candidate_sha,
                "source_sha256":dict(self._pins),"base_overlay":self._base_overlay,
                "target_catalog_sha256":self.target_sha256,
                "expected_preexisting_tables":list(self.expected_preexisting_tables)}


@dataclass(frozen=True)
class CatalogObservation:
    sha256: str
    stdout_sha256: str
    stdout_bytes: int


@dataclass(frozen=True)
class NullableBaselineObservation:
    """Private complete bytes and actual H binding; never a release verdict."""
    raw: bytes = field(repr=False)
    sha256: str
    stdout_sha256: str
    stdout_bytes: int
    source_sha: str
    image_id: str
    recipe_sha256: str


class OwnedCatalog:
    def __init__(self, manager: processes.OwnedProcesses, pg: postgres.SyntheticPostgres, contract: CatalogContract):
        need(type(manager) is processes.OwnedProcesses and type(pg) is postgres.SyntheticPostgres and type(contract) is CatalogContract)
        need(manager.bindings["previous"].source_sha==contract.previous_sha
             and manager.bindings["previous"].reference==contract.previous_web
             and manager.bindings["candidate"].source_sha==contract.candidate_sha, "catalog-image-source-mismatch")
        if getattr(contract, "purpose", None) == "rollback-v0":
            need("catalog-control" in manager.bindings
                 and manager.bindings["candidate"].reference == contract.candidate_web
                 and manager.bindings["previous"].recipe_sha256 == contract.target_recipe.sha256
                 and manager.bindings["candidate"].recipe_sha256 == contract.live_recipe.sha256
                 and manager.catalog_binding.reference == manager.catalog_binding.image_id
                 and manager.catalog_binding.source_sha == contract.catalog_control_sha
                 and manager.bindings["controller"].source_sha == contract.catalog_control_sha
                 and manager.owner.source_sha == contract.catalog_control_sha,
                 "catalog-image-source-mismatch")
        else:
            need("catalog-control" not in manager.bindings, "catalog-image-source-mismatch")
        need(pg.container==manager.owner._ids["pg"] and pg.token==manager.owner.token and pg.runtime_role=="compat_runtime",
             "catalog-pg-binding-invalid")
        self.manager,self.pg,self.contract=manager,pg,contract
        self._failed=False
        self._baseline=None
        self._initial_attempted=False
        self._nullable_attempted=False

    def _quiescent(self):
        need(not self._failed and not self.manager._protocol_failed and self.pg._phase is None
             and not self.manager._ids, "catalog-phase-active")
        self.manager._assert_all()
        self.pg._check()
        result=self.pg._sql(postgres._READ_HEADER+self.pg._state_sql()+"COMMIT;\n")
        need(not result.stderr, "catalog-quiescence-diagnostics")
        rows=postgres._json_lines(result.stdout)
        need(len(rows)==1)
        self.pg._state(rows[0])

    def observe(self, inputs: processes.ConfigInputs) -> CatalogObservation:
        self._quiescent()
        try:
            handle=self.manager.start_catalog(inputs)
            completed=self.manager.wait_catalog(handle)
            # Known cleanup never turns a failed/ambiguous report into success.
            self.manager.remove(handle)
            if type(completed.returncode) is not int or completed.returncode != 0 or completed.stderr:
                code = "catalog-cli-failed"
                if type(completed.returncode) is int and type(completed.stderr) is bytes:
                    code = _CLI_FAILURES.get((completed.returncode, completed.stderr), code)
                raise proof.EvidenceError(code)
            observed=self.contract.parse_compare(completed.stdout)
            self._quiescent()
            return CatalogObservation(observed,hashlib.sha256(completed.stdout).hexdigest(),len(completed.stdout))
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise

    def initial(self, inputs):
        """Observed CLI then complete data capture, checked against source inventory."""
        need(not self._initial_attempted, "catalog-baseline-already-attempted")
        self._initial_attempted=True
        try:
            observed=self.observe(inputs)
            need(observed.sha256==self.contract.target_sha256, "catalog-baseline-mismatch")
            tables=self.pg.capture_tables()
            self.contract.check_initial_tables(tables)
            self._quiescent()
            self._baseline=tables
            return observed,tables
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise

    def observe_nullable_baseline(self, inputs):
        """One fixed baseline graph capture, with cleanup before any return."""
        need(not self._nullable_attempted, "catalog-baseline-already-attempted")
        self._nullable_attempted = True
        try:
            self._quiescent()
            binding = self.manager._nullable_binding()
            need(binding.source_sha == (self.contract.catalog_control_sha
                 if getattr(self.contract, "purpose", None) == "rollback-v0" else self.contract.candidate_sha),
                 "catalog-image-source-mismatch")
            handle = self.manager.start_nullable_baseline(inputs)
            completed = self.manager.wait_nullable_baseline(handle)
            self.manager.remove(handle)
            need(type(completed.returncode) is int and completed.returncode == 0 and not completed.stderr,
                 "catalog-baseline-cli-failed")
            self.contract.parse_nullable_baseline(completed.stdout)
            self._quiescent()
            need(self.manager._nullable_binding() == binding, "catalog-image-source-mismatch")
            return NullableBaselineObservation(completed.stdout, self.contract.target_sha256,
                hashlib.sha256(completed.stdout).hexdigest(), len(completed.stdout),
                binding.source_sha, binding.image_id, binding.recipe_sha256)
        except Exception:
            self._failed = True
            self.manager._protocol_failed = True
            raise

    def after_phase(self, inputs, *, original_columns=False):
        """Caller must have terminated and finished its PG phase first.

        original_columns=True is forward comparison; False is all-column D0
        and N-1. Complete current tables remain present in both projections.
        """
        need(type(original_columns) is bool and self._baseline is not None, "catalog-baseline-missing")
        try:
            observed=self.observe(inputs)
            tables=self.pg.capture_tables(projection=self._baseline if original_columns else None)
            self._quiescent()
            return observed,tables
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise
