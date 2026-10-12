"""Hosted compatibility entry points using the reviewed experiment adapter.

The preview freezes release identities and the retained database origin from
one verified chain. The run repeats that complete source operation and refuses
any difference before building images. Only small canonical public files
leave private scratch. This file does not create or enable a workflow.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import hashlib
import json
import os
import re
import shutil
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat
import images
import proof
import runtime
import schema_change
import compat_evidence
import record_resolver
import record_transport
import pending_binding
import public_evidence

common, records = runtime.common, runtime.release_record
_SHA = public_evidence._SHA
_MAX_PUBLIC = public_evidence._MAX_PUBLIC
_MAX_CONTEXT = public_evidence._MAX_CONTEXT
_WORKFLOW_PATH = public_evidence._WORKFLOW_PATH
_WORKFLOW = public_evidence._WORKFLOW


need = public_evidence.need


canonical = public_evidence.canonical


Context = public_evidence.Context


context = public_evidence.context


regular_path = public_evidence.regular_path


def _identity(entry):
    need(type(entry) is record_resolver.ReleaseSelection, "cli-snapshot-identity")
    return {"release": entry.release, "manifest_sha256": entry.manifest_sha256,
            "sha": entry.record_sha, "web_digest": dict(entry.images)["web"]}


def _resolved_snapshot(resolution, ctx):
    """Project immutable source data; the caller must run the concrete resolver."""
    need(type(resolution) is record_resolver.ForwardResolution
         and resolution.candidate.sha == ctx.sha, "cli-snapshot-context")
    latest, selected = resolution.resolved.latest, resolution.resolved.selected
    live = latest.live
    base = {"mode": "legacy-source-only" if live is None else "retained-live-v2",
            "source_sha": resolution.database_base_source,
            "origin": None if live is None else {"release": live.origin_release,
                       "manifest_sha256": live.origin_manifest_sha256},
            "catalog": None if live is None else live.catalog._asdict()}
    classification = resolution.classification
    contract = compat.catalog.CatalogContract.from_resolution(resolution)
    value = {"version": 4, "sha": ctx.sha, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
             "requested_rollback_target": ctx.target, "latest": _identity(latest), "selected": _identity(selected),
             "database_base": base, "selected_web_source_sha": resolution.previous_web_source_sha,
             "pending": pending_binding.to_wire(contract.pending_binding),
             "preview": {"kind": classification.kind, "dormant": classification.dormant,
                         "blocked": bool(classification.reasons) or ctx.target is not None
                         or classification.requirements != ("compat-required", "generated-migration-closure-ci")}}
    return validate_snapshot(value, ctx)


validate_snapshot = public_evidence.validate_snapshot


def preview(repo, ctx, client, *, git_binary, deadline):
    """One closed authenticated source operation inside the caller's clock."""
    need(type(deadline) is images._Deadline, "cli-deadline-invalid")
    deadline.remaining()
    resolution = record_resolver.resolve_forward(client, repo, git_binary, ctx.sha, selected_tag=ctx.target)
    deadline.remaining()
    return _resolved_snapshot(resolution, ctx)


validate_result = public_evidence.validate_result


evidence_context = public_evidence.evidence_context


validate_evidence = public_evidence.validate_evidence


accepted = public_evidence.accepted


def execute(repo, ctx, client, snapshot, *, git_binary, deadline,
            docker_binary, config_dir, image_work_dir, buildx_binary):
    validate_snapshot(snapshot, ctx)
    need(ctx.target is None, "cli-target-proof-unsupported")
    need(snapshot["preview"]["blocked"] is False, "cli-preview-blocked")
    need(type(deadline) is images._Deadline, "cli-deadline-invalid")
    deadline.remaining()
    resolution = record_resolver.resolve_forward(client, repo, git_binary, ctx.sha)
    need(canonical(_resolved_snapshot(resolution, ctx)) == canonical(snapshot), "cli-resolution-changed")
    deadline.remaining()
    raw = compat._run_resolved_experiment(repo, resolution, ctx.run_id, ctx.attempt, deadline=deadline,
        docker_binary=docker_binary, config_dir=config_dir, image_work_dir=image_work_dir,
        buildx_binary=buildx_binary)
    validate_result(raw, snapshot, ctx)
    return raw  # An actual false result is valid evidence, not a passing gate.


_rollback_context = public_evidence._rollback_context


def _rollback_snapshot(resolution, ctx):
    """Derive public bindings only after complete source-material replay.

    This private projection and its types are not authentication capabilities;
    the named preview/run wrappers obtain their source facts concretely.
    """
    _rollback_context(ctx)
    need(type(resolution) is record_resolver.RollbackProofResolution, "cli-snapshot-context")
    contract = compat.catalog.CatalogContract.from_rollback_resolution(resolution)
    latest, selected = resolution.resolved.latest, resolution.resolved.selected
    need(resolution.producer.sha == resolution.resolved.inventory.main_sha == ctx.sha
         and contract.catalog_control_sha == ctx.sha
         and selected.release == ctx.target, "cli-snapshot-context")
    live, required = latest.live, resolution.requirements
    try:
        material = record_resolver.rollback_material_digest(resolution)
    except record_resolver.policy.PolicyRefusal:
        raise proof.EvidenceError("cli-snapshot-invalid") from None
    value = {"version": 1, "purpose": "rollback-v0", "sha": ctx.sha,
        "run_id": ctx.run_id, "run_attempt": ctx.attempt, "requested_rollback_target": ctx.target,
        "latest": _identity(latest), "selected": _identity(selected),
        "preview": {"kind": "none", "dormant": False, "blocked": False},
        "database_base": {"mode": "retained-live-v2", "source_sha": live.origin_source.sha,
            "origin": {"release": live.origin_release, "manifest_sha256": live.origin_manifest_sha256},
            "catalog": live.catalog._asdict()},
        "selected_web_source_sha": selected.actual_web_source_sha,
        "live_web_source_sha": latest.actual_web_source_sha,
        "span": {"version": 1, "records": len(required.span),
                 "non_none": required.non_none_count, "sha256": required.span_sha256},
        "source_material_sha256": material}
    return validate_rollback_snapshot(value, ctx)


validate_rollback_snapshot = public_evidence.validate_rollback_snapshot


validate_rollback_result = public_evidence.validate_rollback_result


rollback_evidence_context = public_evidence.rollback_evidence_context


validate_rollback_evidence = public_evidence.validate_rollback_evidence


def _preview_rollback(repo, ctx, client, *, git_binary, deadline):
    """Concrete rollback preview for an already validated explicit target."""
    _rollback_context(ctx)
    need(type(deadline) is images._Deadline, "cli-deadline-invalid")
    deadline.remaining()
    resolution = record_resolver.resolve_rollback_proof(client, repo, git_binary, ctx.sha, ctx.target)
    deadline.remaining()
    value = _rollback_snapshot(resolution, ctx)
    deadline.remaining()
    return value


def _execute_rollback(repo, ctx, client, snapshot, *, git_binary, deadline,
                      docker_binary, config_dir, image_work_dir, buildx_binary):
    """Freshly resolve before the private actual engine; no new budget."""
    validate_rollback_snapshot(snapshot, ctx)
    need(type(deadline) is images._Deadline, "cli-deadline-invalid")
    deadline.remaining()
    resolution = record_resolver.resolve_rollback_proof(client, repo, git_binary, ctx.sha, ctx.target)
    need(canonical(_rollback_snapshot(resolution, ctx)) == canonical(snapshot), "cli-resolution-changed")
    deadline.remaining()
    raw = compat._run_rollback_resolved_experiment(repo, resolution, ctx.run_id, ctx.attempt, deadline=deadline,
        docker_binary=docker_binary, config_dir=config_dir, image_work_dir=image_work_dir,
        buildx_binary=buildx_binary)
    validate_rollback_result(raw, snapshot, ctx)
    deadline.remaining()
    return raw


def _read_public(path, *, limit=_MAX_PUBLIC):
    regular_path(path)
    with path.open("rb") as stream:
        raw = stream.read(limit + 1)
    need(0 < len(raw) <= limit, "cli-public-bound")
    return raw


def _write_public(path, raw):
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_PUBLIC, "cli-public-bound")
    regular_path(path.parent, directory=True)
    need(not path.exists() and not path.is_symlink(), "cli-public-exists")
    # A partial write never qualifies: caller/aggregator demands canonical
    # complete bytes and this process must succeed before upload is permitted.
    with path.open("xb") as stream:
        stream.write(raw)


def _write_evidence(directory, result, context_raw, snapshot, ctx):
    validate_evidence(context_raw, result, snapshot, ctx)
    _write_public(directory / "compat-result.json", result)
    _write_public(directory / "compat-context.json", context_raw)
    need({p.name for p in directory.iterdir()} == {"compat-result.json", "compat-context.json"},
         "cli-evidence-inventory")
    written_result = _read_public(directory / "compat-result.json")
    written_context = _read_public(directory / "compat-context.json", limit=_MAX_CONTEXT)
    need(written_result == result and written_context == context_raw, "cli-evidence-written")
    validate_evidence(written_context, written_result, snapshot, ctx)


def _write_rollback_evidence(directory, result, context_raw, snapshot, ctx):
    validate_rollback_evidence(context_raw, result, snapshot, ctx)
    _write_public(directory / "compat-result.json", result)
    _write_public(directory / "compat-context.json", context_raw)
    need({p.name for p in directory.iterdir()} == {"compat-result.json", "compat-context.json"},
         "cli-evidence-inventory")
    written_result = _read_public(directory / "compat-result.json")
    written_context = _read_public(directory / "compat-context.json", limit=_MAX_CONTEXT)
    need(written_result == result and written_context == context_raw, "cli-evidence-written")
    validate_rollback_evidence(written_context, written_result, snapshot, ctx)


def _mark_complete(env):
    """A fixed step output permits uploading a measured negative verdict.

    The workflow may upload only after this marker, even if check returns one
    for actual false flags. Infrastructure and partial writes never mark it.
    """
    value = env.get("GITHUB_OUTPUT")
    if value is None:
        return
    path = regular_path(Path(value))
    root = regular_path(Path(env.get("RUNNER_TEMP", "")), directory=True)
    need(path.is_relative_to(root), "cli-output-path-invalid")
    common.Output(stdout=sys.stdout, output_path=str(path)).set_outputs({"complete": "true"})


def _scratch(repo, ctx, phase, env):
    root = regular_path(Path(env.get("RUNNER_TEMP", "")), directory=True)
    need(not root.is_relative_to(repo) and not repo.is_relative_to(root), "cli-scratch-overlap")
    work = root / ("compat-" + phase + "-" + ctx.suffix)
    need(not work.exists(), "cli-already-attempted")
    work.mkdir(mode=0o700)
    for name in ("gh-config", "gh-work", "docker-config", "images", "public"):
        (work / name).mkdir(mode=0o700)
    return work


def _git_binary(env):
    value = env.get("COMPAT_GIT_BIN") or shutil.which("git")
    need(type(value) is str and value, "cli-git-unavailable")
    return regular_path(Path(value))


def _client(work, env, deadline):
    need(type(deadline) is images._Deadline, "cli-deadline-invalid")
    token = env.get("GH_TOKEN")
    need(type(token) is str and 0 < len(token) <= 8192 and not any(c in token for c in "\r\n\0"),
         "cli-token-unavailable")
    binary = regular_path(Path(env.get("COMPAT_GH_BIN", "")))
    def bounded_capture(argv, **options):
        if argv == [str(binary), "--version"]:
            options["timeout"] = min(options["timeout"], 5)
        return runtime.bounded_command(argv, **options)
    budget = record_transport.OperationBudget(deadline.end, capture=bounded_capture, clock=deadline.clock)
    try:
        return record_transport.ReleaseGh(binary, token, work / "gh-config", work / "gh-work",
            budget=budget, base_env=common.scrubbed_env(base=env))
    except Exception:
        budget.close()
        raise


# Only fixed reviewed error identifiers may leave the experiment. Unknown
# exceptions retain the generic refusal; never format exception text, paths,
# subprocess streams, credentials, or SQL into public diagnostics.
_PUBLIC_FAILURE_CODES = frozenset({
    "admission-already-attempted", "admission-blob-changed", "admission-blob-frame",
    "admission-build-capture", "admission-build-config-changed", "admission-build-containment",
    "admission-build-descendants", "admission-build-environment-changed", "admission-build-failed",
    "admission-build-handle-inherited", "admission-build-job-close", "admission-build-job-exists",
    "admission-build-job-inventory", "admission-build-job-query", "admission-build-job-terminate",
    "admission-build-output-bound", "admission-build-pipe", "admission-build-process-handle",
    "admission-build-process-membership", "admission-build-process-wait", "admission-build-reserve",
    "admission-build-search-path", "admission-build-start", "admission-build-teardown-unresolved",
    "admission-build-unknown", "admission-builder-already-exists", "admission-builder-bootstrap",
    "admission-builder-capture", "admission-builder-changed", "admission-builder-cleanup-deadline",
    "admission-builder-cleanup-unresolved", "admission-builder-command", "admission-builder-container",
    "admission-builder-container-changed", "admission-builder-created", "admission-builder-id",
    "admission-builder-image", "admission-builder-location", "admission-builder-metadata",
    "admission-builder-mode", "admission-builder-mount", "admission-builder-mutation-unbound",
    "admission-builder-node", "admission-builder-ownership-unresolved", "admission-builder-removal-unproven",
    "admission-builder-restarted", "admission-builder-state", "admission-builder-unbound",
    "admission-builder-unproven", "admission-builder-volume", "admission-builder-volume-changed",
    "admission-built-image-changed", "admission-clock-invalid", "admission-context-changed",
    "admission-context-exists", "admission-controller-inputs", "admission-credential-helper-available",
    "admission-deadline", "admission-docker-capture", "admission-docker-location", "admission-docker-prefix",
    "admission-engine-changed", "admission-engine-invalid", "admission-engine-unavailable",
    "admission-experiment-reserve", "admission-file-bound", "admission-file-invalid",
    "admission-git-override", "admission-graph-inputs", "admission-head-changed",
    "admission-ignore-override", "admission-ignore-unsupported", "admission-iid-invalid",
    "admission-iid-unbound", "admission-image-unavailable", "admission-incomplete",
    "admission-infrastructure-image-changed", "admission-path-invalid", "admission-platform-unsupported",
    "admission-previous-invalid", "admission-recipe-unsupported", "admission-release-client-invalid",
    "admission-role-image-invalid", "admission-role-label-invalid", "admission-source-changed",
    "admission-source-dirty", "admission-source-mode", "admission-source-read", "admission-system-directory",
    "admission-transport-in-use", "admission-transport-replaced", "admission-tree-invalid",
    "catalog-base-overlay-unsupported", "catalog-baseline-already-attempted", "catalog-baseline-mismatch",
    "catalog-baseline-missing", "catalog-cli-capture-failed", "catalog-cli-configuration-unavailable",
    "catalog-cli-failed", "catalog-cli-invalid-arguments", "catalog-cli-output-failed",
    "catalog-contract-invalid", "catalog-fallback-not-equal", "catalog-image-source-mismatch",
    "catalog-json-invalid", "catalog-output-invalid", "catalog-ownership-invalid",
    "catalog-pending-invalid", "catalog-pending-unsupported",
    "catalog-pg-binding-invalid", "catalog-phase-active", "catalog-progression-unsupported",
    "catalog-quiescence-diagnostics", "catalog-source", "catalog-source-invalid",
    "catalog-table-inventory-mismatch", "catalog-unknown-unsupported", "experiment-already-attempted",
    "experiment-budget-in-use", "experiment-builder-location-invalid",
    "experiment-classification-unsupported", "experiment-cleanup", "experiment-cleanup-transport-unowned",
    "experiment-clock-invalid", "experiment-concurrent-transport", "experiment-deadline",
    "experiment-exit-invalid", "experiment-fixture-binding", "experiment-image-binding",
    "experiment-image-work-required", "experiment-input-invalid", "experiment-journal-unsealed",
    "experiment-observer-bridge-invalid", "experiment-old-period-incomplete", "experiment-operation",
    "experiment-overall-invalid", "experiment-pg-not-ready", "experiment-previous-expectation-invalid",
    "experiment-previous-expectation-mismatch", "experiment-previous-seed-failed",
    "experiment-process-still-present", "experiment-readiness-invalid",
    "experiment-source-build-admission-unreviewed", "experiment-source-build-unproven",
    "experiment-status-settle-incomplete", "experiment-terminal-source", "experiment-transport-invalid",
    "runtime-input-invalid",
})


def _failure_message(error):
    args = BaseException.args.__get__(error)
    if isinstance(error, proof.EvidenceError) and len(args) == 1:
        code = args[0]
        if type(code) is str and code in _PUBLIC_FAILURE_CODES:
            return "compatibility-entrypoint-refused: " + code
    return "compatibility-entrypoint-refused"


# Diagnostic-only additions. Keep the original first-line contract above.
# These are reviewed literals, never prefixes, exception text or runtime
# discovery of code strings. ReleaseError accepts arbitrary bounded suffixes,
# so its type alone is insufficient authority to publish its args/code.
_DIAGNOSTIC_EVIDENCE_CODES = _PUBLIC_FAILURE_CODES | frozenset({
    "release-verification-failed", "rollback-target-missing", "gh-read-command-refused", "gh-write-refused",
    "network", "pull", "capture", "container", "process", "boot-timeout", "image-admission-invalid",
    "cli-input-invalid", "cli-repository-invalid", "cli-runner-invalid", "cli-source-invalid",
    "cli-ref-invalid", "cli-event-invalid", "cli-workflow-invalid", "cli-run-invalid",
    "cli-attempt-invalid", "cli-target-invalid", "cli-path-invalid", "cli-snapshot-invalid",
    "cli-snapshot-context", "cli-snapshot-identity", "cli-snapshot-selection", "cli-snapshot-preview",
    "cli-target-missing", "cli-result-invalid", "cli-result-noncanonical", "cli-result-context",
    "cli-preview-blocked", "cli-public-bound", "cli-public-exists", "cli-output-path-invalid",
    "cli-scratch-overlap", "cli-already-attempted", "cli-token-unavailable", "cli-constructor-command-invalid",
    "cli-command-invalid", "cli-experiment-disabled", "cli-snapshot-noncanonical", "cli-upstream-failed",
    "cli-snapshot-base", "cli-snapshot-source", "cli-snapshot-pending", "cli-resolution-changed", "cli-deadline-invalid",
    "cli-git-unavailable", "cli-target-proof-unsupported",
})
_DIAGNOSTIC_RELEASE_CODES = frozenset({
    "gh-failed:binary", "gh-failed:token", "gh-failed:repository", "gh-failed:version",
    "gh-failed:releases", "gh-failed:tags", "gh-failed:download",
    "attestation-unverified:record", "attestation-unverified:image", "attestation-unverified:legacy-predicate",
    "record-chain-invalid:bootstrap", "record-chain-invalid:link", "record-chain-invalid:release",
    "record-chain-invalid:tag-format", "record-chain-invalid:release-state", "record-chain-invalid:duplicate",
    "record-chain-invalid:assets", "record-chain-invalid:asset-inventory", "record-chain-invalid:asset-size",
    "record-chain-invalid:tag", "record-chain-invalid:tag-release-parity", "record-chain-invalid:too-many",
    "record-chain-invalid:download", "record-chain-invalid:manifest", "record-chain-invalid:tag-binding",
    "record-chain-invalid:attestation", "record-invalid", "downgrade-refused:unknown-commit",
    "git-failed:merge-base", "input-invalid:json", "input-invalid:json-duplicate-key", "input-invalid:json-number",
})
_DIAGNOSTIC_SOURCE_CODES = frozenset({
    "git-failed:source-deadline", "git-failed:source-budget",
    "git-failed:source-capture", "git-failed:source-command",
})
_DIAGNOSTIC_TRANSPORT_CODES = frozenset({
    "record-chain-invalid:transport-deadline", "record-chain-invalid:transport-bound",
    "record-chain-invalid:transport-capture", "record-chain-invalid:transport-resolver",
})


def _diagnostic_code(error):
    args = BaseException.args.__get__(error)
    allowed = (_DIAGNOSTIC_SOURCE_CODES if type(error) is record_resolver.source_git.SourceGitRefusal
               else _DIAGNOSTIC_TRANSPORT_CODES if type(error) is record_transport.TransportRefusal
               else _DIAGNOSTIC_EVIDENCE_CODES if isinstance(error, proof.EvidenceError)
               else _DIAGNOSTIC_RELEASE_CODES if isinstance(error, common.ReleaseError) else ())
    return args[0] if len(args) == 1 and type(args[0]) is str and args[0] in allowed else "unknown"


def _diagnostic_family(error):
    for kind, family in ((proof.InfrastructureFailure, "infrastructure"), (proof.EvidenceError, "evidence"),
                         (common.ReleaseError, "release"), (OSError, "os"), (ValueError, "value"),
                         (TypeError, "type"), (KeyError, "key")):
        if isinstance(error, kind):
            return family
    return "other"


def _diagnostic_phase(error):
    phase = "unknown"
    trace = BaseException.__traceback__.__get__(error)
    for _ in range(64):
        if trace is None:
            return phase
        # Identity, not function name, filename, source text or code equality.
        # The deepest explicitly mapped owner wins; generic transports do not
        # erase the surrounding verification/pull/build/measurement phase.
        for code, label in _DIAGNOSTIC_PHASES:
            if trace.tb_frame.f_code is code:
                phase = label
                break
        trace = trace.tb_next
    return "trace-limit" if trace is not None else phase


def _failure_diagnostic(error):
    parts, seen = [], set()
    for _ in range(4):
        if error is None:
            parts.append("chain=complete")
            break
        if id(error) in seen:
            parts.append("chain=cycle")
            break
        seen.add(id(error))
        parts.append("phase=" + _diagnostic_phase(error) + " family=" + _diagnostic_family(error)
                     + " code=" + _diagnostic_code(error))
        # A suppressed context still carries a fixed diagnostic cause. Read
        # BaseException slots directly: no custom properties, str or repr.
        cause = BaseException.__cause__.__get__(error)
        error = cause if cause is not None else BaseException.__context__.__get__(error)
    else:
        parts.append("chain=complete" if error is None else "chain=limit")
    return "compatibility-diagnostic: " + "; ".join(parts)


def _build_failure_diagnostic(error):
    seen = set()
    for _ in range(4):
        if error is None or id(error) in seen:
            return None
        seen.add(id(error))
        if type(error) is images._BuildFailure:
            value = images._BuildFailure._public(error)
            if value is None:
                return None
            raw = canonical(value)
            if len(raw) <= 1024:
                return "compatibility-build-diagnostic: " + raw.decode('ascii').rstrip('\n')
            return None
        cause = BaseException.__cause__.__get__(error)
        error = cause if cause is not None else BaseException.__context__.__get__(error)
    return None


def main(argv=None):
    try:
        args = sys.argv[1:] if argv is None else argv
        need(type(args) is list and len(args) == 1 and type(args[0]) is str
             and args[0] in {"preview", "run", "check", "validate-evidence"}, "cli-command-invalid")
        command = args[0]
        need(images._READY_FOR_EXECUTION, "cli-experiment-disabled")
        ctx = context(os.environ)
        if ctx.target is not None:
            _rollback_context(ctx)
        repo = regular_path(Path.cwd().resolve(), directory=True)
        root = regular_path(Path(os.environ.get("RUNNER_TEMP", "")), directory=True)
        if command == "preview":
            deadline = images._Deadline()
            # Complete historical image verification and immutable-source
            # classification share the existing source-operation 600s ceiling.
            # Establish it before scratch/client work; never reset it later.
            deadline.end = min(deadline.end, deadline.now() + 600)
            work = _scratch(repo, ctx, "preview", os.environ)
            git_binary = _git_binary(os.environ)
            if ctx.target is None:
                value = preview(repo, ctx, _client(work, os.environ, deadline),
                    git_binary=git_binary, deadline=deadline)
            else:
                value = _preview_rollback(repo, ctx, _client(work, os.environ, deadline),
                    git_binary=git_binary, deadline=deadline)
            _write_public(work / "public" / "resolution.json", canonical(value))
            return 1 if value["preview"]["blocked"] else 0
        raw = _read_public(root / "compat-input" / "resolution.json")
        snapshot = common.loads_strict(raw)
        if ctx.target is None:
            need(canonical(validate_snapshot(snapshot, ctx)) == raw, "cli-snapshot-noncanonical")
        else:
            need(canonical(validate_rollback_snapshot(snapshot, ctx)) == raw, "cli-snapshot-noncanonical")
        if command in {"check", "validate-evidence"}:
            need(os.environ.get("COMPAT_PREVIEW_OUTCOME") == "success"
                 and os.environ.get("COMPAT_RUN_OUTCOME") == "success", "cli-upstream-failed")
            result = _read_public(root / "compat-input" / "compat-result.json")
            if ctx.target is None:
                value = validate_result(result, snapshot, ctx)
            else:
                value = validate_rollback_result(result, snapshot, ctx)
            if command == "validate-evidence":
                need({p.name for p in (root / "compat-input").iterdir()}
                     == {"resolution.json", "compat-result.json", "compat-context.json"},
                     "cli-evidence-inventory")
                context_raw = _read_public(root / "compat-input" / "compat-context.json", limit=_MAX_CONTEXT)
                if ctx.target is None:
                    validate_evidence(context_raw, result, snapshot, ctx)
                else:
                    validate_rollback_evidence(context_raw, result, snapshot, ctx)
            else:
                if ctx.target is None:
                    context_raw = canonical(evidence_context(result, snapshot, ctx))
                else:
                    context_raw = canonical(rollback_evidence_context(result, snapshot, ctx))
            work = _scratch(repo, ctx, command, os.environ)
            if ctx.target is None:
                _write_evidence(work / "public", result, context_raw, snapshot, ctx)
            else:
                _write_rollback_evidence(work / "public", result, context_raw, snapshot, ctx)
            _mark_complete(os.environ)
            return 0 if command == "validate-evidence" or accepted(value) else 1
        need(snapshot["preview"]["blocked"] is False, "cli-preview-blocked")
        deadline = images._Deadline()
        work = _scratch(repo, ctx, "run", os.environ)
        docker = regular_path(Path(os.environ.get("COMPAT_DOCKER_BIN", "")))
        plugin = regular_path(Path(os.environ.get("COMPAT_BUILDX_BIN", "")))
        git_binary = _git_binary(os.environ)
        if ctx.target is None:
            result = execute(repo, ctx, _client(work, os.environ, deadline), snapshot,
                git_binary=git_binary, deadline=deadline, docker_binary=str(docker),
                config_dir=work / "docker-config", image_work_dir=work / "images", buildx_binary=plugin)
        else:
            result = _execute_rollback(repo, ctx, _client(work, os.environ, deadline), snapshot,
                git_binary=git_binary, deadline=deadline, docker_binary=str(docker),
                config_dir=work / "docker-config", image_work_dir=work / "images", buildx_binary=plugin)
        _write_public(work / "public" / "compat-result.json", result)
        return 0
    except (proof.EvidenceError, common.ReleaseError, record_resolver.policy.PolicyRefusal,
            OSError, ValueError, TypeError, KeyError) as error:
        print(_failure_message(error), file=sys.stderr)
        print(_failure_diagnostic(error), file=sys.stderr)
        build_diagnostic = _build_failure_diagnostic(error)
        if build_diagnostic is not None:
            print(build_diagnostic, file=sys.stderr)
        return 1


# Freeze code identities at import; do not inspect traceback filenames/locals
# or dynamically enumerate modules. New owners require an explicit review.
_DIAGNOSTIC_PHASES = tuple((function.__code__, phase) for function, phase in (
    (main, "cli-entry"), (context, "cli-context"), (_scratch, "cli-scratch"), (_client, "cli-client"),
    (regular_path, "cli-path"), (_read_public, "cli-read"), (_write_public, "cli-write"),
    (_mark_complete, "cli-complete"), (validate_snapshot, "cli-snapshot"), (validate_result, "cli-result"),
    (preview, "preview"), (execute, "execute"),
    (_rollback_context, "cli-context"), (_rollback_snapshot, "cli-snapshot"),
    (validate_rollback_snapshot, "cli-snapshot"), (validate_rollback_result, "cli-result"),
    (_preview_rollback, "preview"), (_execute_rollback, "execute"),
    (_write_rollback_evidence, "cli-write"), (validate_rollback_evidence, "cli-result"),
    (_resolved_snapshot, "cli-snapshot"), (_git_binary, "cli-path"),
    (record_resolver.resolve_forward, "release-verify"), (record_resolver._resolve, "release-verify"),
    (record_transport.ReleaseGh.__init__, "release-client"),
    (record_transport.OperationBudget.capture, "release-capture"),
    (record_transport.OperationBudget.remaining, "release-budget"),
    (record_resolver.source_git.ImmutableGit._call, "source-capture"),
    (record_resolver.source_git.ImmutableGit._check, "source-budget"),
    (runtime.verified_previous, "release-verify"), (runtime.ReadOnlyGh.__init__, "release-client"),
    (runtime.ReadOnlyGh._run, "release-transport"), (runtime.ReadOnlyGh.download_manifest, "release-download"),
    (runtime.ReadOnlyGh.verify_file, "release-record-attestation"),
    (runtime.ReadOnlyGh.verify_image, "release-image-attestation"),
    (records.resolve_chain, "release-chain"), (records.load_bootstrap, "release-bootstrap"),
    (records.verify_images, "release-images"), (schema_change.classify, "classification"),
    (compat.catalog.CatalogContract.__init__, "catalog-contract"),
    (compat.catalog.CatalogContract.from_resolution.__func__, "catalog-contract"),
    (compat.catalog.OwnedCatalog.observe, "catalog-observe"),
    (images._Source.clean, "source-clean"), (images._Source.inventory, "source-inventory"),
    (images._Source.blobs, "source-export"), (images._admit, "image-admission"),
    (images._admit_resolved, "image-admission"), (images._admit_core, "image-admission"),
    (images._build_environment, "build-environment"), (images._build_capture, "image-build"),
    (images._LinuxBuilder.start, "builder-start"), (images._LinuxBuilder.close, "builder-cleanup"),
    (images._role_inspect, "image-inspect"), (runtime.Docker.pull, "docker-pull"),
    (runtime.Docker.inspect, "docker-inspect"), (compat.run_experiment, "experiment-entry"),
    (compat._run_resolved_experiment, "experiment-entry"),
    (compat._Engine.run, "experiment"), (compat._Engine._measure, "measurement"),
    (compat._Engine._server, "server"), (compat._Engine._old_server, "old-server"),
    (compat._Engine._migration, "migration"), (compat._Engine._settle, "journal-settle"),
    (compat._Engine._cleanup, "cleanup"), (compat._wait_pg, "pg-readiness"),
))


if __name__ == "__main__":
    raise SystemExit(main())
