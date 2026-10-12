"""Prepare the two fixed signing subjects from complete public evidence.

This local wire/file boundary makes no success or release-authority claim.
Upstream experiment readiness remains in preview/run; no experiment module is
imported here. A complete measured negative is deliberately still preparable.
"""
from __future__ import annotations

import os
from pathlib import Path
import sys

# The checkout location is fixed by the workflow. No discovery or CLI import.
sys.path.insert(0, str(Path(__file__).parent.parent / "deployment"))
sys.path.insert(0, str(Path(__file__).parent))
import public_evidence as public

proof, common = public.proof, public.common
need, canonical, context, regular_path = public.need, public.canonical, public.context, public.regular_path
_MAX_PUBLIC, _MAX_CONTEXT = public._MAX_PUBLIC, public._MAX_CONTEXT
validate_snapshot, validate_result = public.validate_snapshot, public.validate_result
validate_evidence = public.validate_evidence
validate_rollback_snapshot, validate_rollback_result = public.validate_rollback_snapshot, public.validate_rollback_result
validate_rollback_evidence = public.validate_rollback_evidence


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


def main(argv=None):
    try:
        args = sys.argv[1:] if argv is None else argv
        need(type(args) is list and not args, "cli-command-invalid")
        ctx = context(os.environ)
        if ctx.target is not None:
            public._rollback_context(ctx)
        repo = regular_path(Path.cwd().resolve(), directory=True)
        root = regular_path(Path(os.environ.get("RUNNER_TEMP", "")), directory=True)
        raw = _read_public(root / "compat-input" / "resolution.json")
        snapshot = common.loads_strict(raw)
        if ctx.target is None:
            need(canonical(validate_snapshot(snapshot, ctx)) == raw, "cli-snapshot-noncanonical")
        else:
            need(canonical(validate_rollback_snapshot(snapshot, ctx)) == raw, "cli-snapshot-noncanonical")
        need(os.environ.get("COMPAT_PREVIEW_OUTCOME") == "success"
             and os.environ.get("COMPAT_RUN_OUTCOME") == "success", "cli-upstream-failed")
        result = _read_public(root / "compat-input" / "compat-result.json")
        if ctx.target is None:
            validate_result(result, snapshot, ctx)
        else:
            validate_rollback_result(result, snapshot, ctx)
        need({p.name for p in (root / "compat-input").iterdir()}
             == {"resolution.json", "compat-result.json", "compat-context.json"},
             "cli-evidence-inventory")
        context_raw = _read_public(root / "compat-input" / "compat-context.json", limit=_MAX_CONTEXT)
        if ctx.target is None:
            validate_evidence(context_raw, result, snapshot, ctx)
        else:
            validate_rollback_evidence(context_raw, result, snapshot, ctx)
        work = _scratch(repo, ctx, "validate-evidence", os.environ)
        if ctx.target is None:
            _write_evidence(work / "public", result, context_raw, snapshot, ctx)
        else:
            _write_rollback_evidence(work / "public", result, context_raw, snapshot, ctx)
        _mark_complete(os.environ)
        return 0
    except (proof.EvidenceError, common.ReleaseError, OSError, ValueError, TypeError, KeyError):
        # Never include exception text, local bytes, paths or private diagnostics.
        print("attest-evidence-refused", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
