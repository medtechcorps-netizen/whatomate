"""Source and record integration for the protected Release workflow.

The public read adapters establish authority. These helpers do not accept an
external receipt in place of a fresh read. Promotion inputs cross jobs only
through the successful production job's fixed needs outputs in ship.yml.
"""
from dataclasses import dataclass
from functools import wraps
from pathlib import Path
import shutil
import time
import uuid

import compat_evidence
import record_handoff
import record_resolver
import record_writer
import ship_compat
import ship_records
import ship_common as common

records = record_resolver.records
policy = record_resolver.policy
SOURCE_SECONDS = 600
PLAN_SECONDS = 900


def _need(value, code):
    if not value:
        common.fail(code)


def _fixed(function):
    @wraps(function)
    def call(*args, **kwargs):
        try:
            return function(*args, **kwargs)
        except compat_evidence.CompatRefusal as exc:
            code = exc.code if exc.code in ('compat-missing', 'compat-old-server-incompatible',
                'irreversible-needs-fix-forward-mode') else 'compat-stale'
            common.fail('candidate-invalid:' + code)
        except policy.PolicyRefusal:
            common.fail('record-chain-invalid:policy')
        except record_writer.WriterRefusal:
            common.fail('record-invalid:writer')
        except record_handoff.HandoffRefusal:
            common.fail('record-invalid:handoff')
    return call


def _options(ctx, deadline):
    _need(type(deadline) in (int, float) and time.monotonic() < deadline, 'record-chain-invalid:source-deadline')
    binary = shutil.which('git', path=ctx.env.get('PATH'))
    _need(binary is not None, 'record-chain-invalid:git-unavailable')
    return dict(gh_binary=Path(ctx.env.get('SHIP_GH_BIN', '')),
        git_binary=Path(binary).absolute(), token=ctx.env.get('GH_TOKEN', ''),
        scratch_dir=ctx.work_dir('source-reads') / uuid.uuid4().hex, deadline=deadline)


@dataclass(frozen=True, slots=True)
class Gate:
    resolved: record_resolver.ResolvedChain
    normal: ship_compat.NormalPreflight | None
    rollback: record_resolver.RollbackResolution | None

    @property
    def classification(self):
        if self.normal is not None:
            return self.normal.resolution.classification
        return self.rollback.legacy_classification


def _complete_source(resolution, sha):
    sources = tuple(source for source in resolution.resolved.chain.sources if source.sha == sha)
    if len(sources) != 1 or sources[0].mode != 'reviewed-v0':
        return None
    source = sources[0]
    if tuple(path for path, _digest in source.artifacts) != policy.SOURCE_PATHS:
        return None
    return source


def _staging_bypass(result):
    # This is staging bypass with successful normal compat, NOT compat-outage
    # bypass. Use only the internally authenticated preflight projection.
    resolution = result.resolution
    change = resolution.classification
    _need(change.kind == 'none' and not change.dormant and not change.reasons
          and not change.migration_changes, 'schema-change-blocked:staging-bypass')
    # The migration-path check is against the retained database origin. Until
    # this projection carries that check against the latest source as well,
    # refuse their divergence even when all seven artifact hashes are equal.
    _need(resolution.database_base_source == resolution.resolved.latest.record_sha,
          'schema-change-blocked:staging-bypass-source-diverged')
    # Legacy reader0 proves no artifact bytes, so absence is not equality.
    for sha in {resolution.resolved.latest.record_sha, resolution.database_base_source}:
        source = _complete_source(resolution, sha)
        _need(source is not None, 'schema-change-blocked:staging-bypass-source-unavailable')
        _need(source.artifacts == resolution.candidate.artifacts,
              'schema-change-blocked:staging-bypass-artifacts')


@_fixed
def gate(ctx, control, mode, target, *, deadline):
    options = _options(ctx, deadline)
    if mode == 'rollback':
        result = ship_records.rollback(ctx.repo_dir, target, **options)
        return Gate(result.resolved, None, result)
    _need(mode in ('dry-run', 'promote', 'stage', 'promote-without-staging'), 'input-invalid:mode')
    result = ship_compat.preflight(ctx.repo_dir, control['sha'], **options)
    if mode == 'promote-without-staging':
        _staging_bypass(result)
    return Gate(result.resolution.resolved, result, None)


def details(value):
    """Public summary values derived from the already checked gate."""
    latest = value.resolved.latest
    if value.normal is not None:
        qualified = value.normal.qualified
        resolution = value.normal.resolution
        candidate, change = resolution.candidate, resolution.classification
        base = _complete_source(resolution, resolution.database_base_source)
        if base is None:
            changed = 'unavailable (the baseline has no complete artifact facts)'
        else:
            prior = dict(base.artifacts)
            paths = [path for path, digest in candidate.artifacts if prior.get(path) != digest]
            changed = ', '.join(paths) if paths else 'none'
        return [f'- Database change: {change.kind}; dormant: {str(change.dormant).lower()}',
            f'- Reversible: {str(qualified.reversible).lower()}; old_server_ok: {str(qualified.old_server).lower()}',
            f'- Compatibility: ok; run: {qualified.run_id}',
            f'- Catalog: version {candidate.catalog.catalog_version}, minimum reader {candidate.catalog.min_reader_version}',
            f'- Measured catalog SHA256: {candidate.catalog.catalog_sha256}',
            f'- Database source: {resolution.database_base_source}',
            f'- Changed database artifact paths: {changed}',
            '- Other changed source paths: unavailable in the retained preflight projection',
            '- Restore drills: not required for none or dormant changes']
    if latest.live is None:
        return ['- Database history: legacy source checks; measured catalog unavailable',
                '- Database change: none (image-only decision); dormant: false',
                '- Reversible: unproven; old_server_ok: unproven',
                '- Catalog versions and changed artifact paths: unavailable (legacy source guard only)',
                '- Compatibility run: none (legacy rollback guard)',
                '- Record format: schema 1 until the first measured promotion']
    decision = value.rollback.decision
    facts = decision.image_only_facts
    return [f'- Database change: {facts.db_change}; dormant: {str(facts.dormant).lower()}',
        f'- Reversible: {str(facts.reversible).lower()} (image-only decision); old_server_ok: unproven',
        f'- Database changes after the exact target: {decision.non_none_count}',
        f'- Retained catalog: version {decision.live.catalog.catalog_version}, minimum reader {decision.live.catalog.min_reader_version}',
        f'- Target reader version: {decision.target.reader.reader_version}',
        f'- Retained catalog SHA256: {decision.live.catalog.catalog_sha256}',
        '- Compatibility run: none (image-only rollback; no new experiment claimed)',
        '- Changed database artifact paths: none (retained live contract)',
        '- Other changed source paths: unavailable in the retained rollback projection',
        '- Restore drills: not required for rollback or restore']


def rollback_case(live, resolved):
    """Retain the existing image-only drift/reconcile/restore behavior."""
    latest, target = resolved.latest, resolved.selected
    if target.release == latest.release:
        if live == dict(latest.images):
            common.fail('nothing-to-roll-back')
        if any(dict(entry.images) == live for entry in resolved.chain.entries):
            common.fail('drift:live-matches-record')
        return 'restore'
    if live == dict(target.images):
        return 'reconcile'
    if live == dict(latest.images):
        return 'rollback'
    common.fail('drift')


def _execution(control):
    # Production can only execute at attempt 1. A record-only rerun must keep
    # that original identity; it cannot claim a second deployment took place.
    _need(type(control['run_attempt']) is int and 1 <= control['run_attempt'] <= 500,
          'record-invalid:run-attempt')
    return record_writer.WorkflowExecution(common.REPOSITORY, records.SHIP_SIGNER,
        control['workflow_sha'], int(control['run_id']), 1)


@_fixed
def promotion_outputs(value, control, verified_images, *, staging, private=()):
    """Call only after candidate image verification and before any provider I/O.

    Complete encode/decode/size validation happens here. Only emit the returned
    mapping after a successful deployment; failed deployments write no record.
    """
    _need(value.normal is not None and control['run_attempt'] == 1, 'record-invalid:production-handoff')
    sha = control['sha']
    images = record_writer.CandidateImages(sha,
        tuple((part, verified_images[part]) for part in common.COMPONENTS),
        tuple((part, sha) for part in common.COMPONENTS))
    handoff = record_writer.prepare_promotion(value.normal.resolution, value.normal.collected,
        images, _execution(control), staging=staging)
    outputs = record_handoff.encode(handoff)
    for key, text in outputs.items():
        _need(common.OUTPUT_KEY_RE.fullmatch(key) and common.OUTPUT_VALUE_RE.fullmatch(text),
              'record-invalid:production-handoff')
        if text:
            common.require_public_text(text, private)
    return outputs


@_fixed
def read(ctx, *, deadline):
    return ship_records.read(ctx.repo_dir, **_options(ctx, deadline))


def _base_matches(manifest, fields):
    expected = common.loads_strict(records.build_manifest(fields))
    # These three fields describe the format/database policy, not the actual
    # production outcome. The v2 writer derives them from authenticated inputs.
    return all(manifest.get(key) == value for key, value in expected.items()
               if key not in ('schema_version', 'db_change', 'schema_guard'))


def _publication(control, fields):
    return record_writer.PublicationFacts(_execution(control), fields['deployed_at'], fields['built_at'],
        fields['environment_values_sha256'], fields['non_source_projection_sha256'])


@_fixed
def record_bytes(ctx, control, fields, *, deadline):
    """Resolve exact published idempotency before needing a promotion handoff.

    Each stage performs its own fresh read. The caller cannot supply a resolved
    chain or replay a cached source receipt.
    """
    resolved = read(ctx, deadline=deadline)
    matches = tuple(entry for entry in resolved.chain.entries if entry.release == fields['release'])
    if matches:
        _need(matches == (resolved.chain.entries[-1],), 'latest-changed-since-production')
        raw = matches[0].record.raw
        manifest = records.validate_manifest(common.loads_strict(raw))
        _need(_base_matches(manifest, fields), 'record-invalid:published-differs')
        _need(record_writer.published_state(resolved, raw) == 'published', 'record-invalid:published-differs')
        return raw, 'published'
    _need(resolved.latest.release == fields['previous'], 'latest-changed-since-production')
    publication = _publication(control, fields)
    if fields['kind'] == 'promote':
        outputs = {key: ctx.env.get('REC_' + key.upper(), '') for key in record_handoff.OUTPUT_KEYS}
        handoff = record_handoff.decode(outputs, workflow_sha=control['workflow_sha'],
            candidate_sha=fields['sha'], run_id=int(control['run_id']), record_attempt=control['run_attempt'])
        raw = record_writer.build_promotion(handoff, publication, resolved)
    else:
        tag = ctx.env.get('SHIP_TARGET_RELEASE', '')
        _need(tag == fields['previous'] if fields['kind'] == 'restore' else tag != fields['previous'],
              'record-invalid:target')
        # Source-bound legacy guard and v2 exact-span policy execute within the
        # same original stage deadline. No new budget is granted for this read.
        rollback = ship_records.rollback(ctx.repo_dir, tag, **_options(ctx, deadline))
        _need(rollback.resolved.chain == resolved.chain
              and rollback.resolved.image_bindings == resolved.image_bindings,
              'latest-changed-since-production')
        if rollback.resolved.latest.live is None:
            _need(rollback.legacy_chain is not None, 'record-invalid:legacy-transition')
            target = rollback.resolved.selected
            _need((fields['sha'], fields['images']) == (target.record_sha, dict(target.images)), 'record-invalid:target')
            raw = records.build_manifest(fields)
            records.require_link(list(rollback.legacy_chain.entries),
                records.manifest_entry(common.loads_strict(raw), raw), None)
        else:
            raw = record_writer.build_image_only(rollback.resolved, publication, reconciled=fields['reconciled'])
    _need(_base_matches(common.loads_strict(raw), fields), 'record-invalid:production-differs')
    _need(time.monotonic() < deadline, 'record-chain-invalid:source-deadline')
    return raw, 'absent'


@_fixed
def verify_published(ctx, raw, *, deadline):
    resolved = read(ctx, deadline=deadline)
    _need(record_writer.published_state(resolved, raw) == 'published', 'record-invalid:not-latest')
