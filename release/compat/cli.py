"""Hosted compatibility entry points using the reviewed experiment adapter.

The preview freezes latest and selected release identities from one verified
chain. The run re-verifies the selected immutable tag and constrains its SHA;
it never resolves a second moving latest. Only small canonical public files
leave private scratch. This file does not create or enable a workflow.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import json
import os
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import compat
import images
import proof
import runtime
import schema_change

common, records = runtime.common, runtime.release_record
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_MAX_PUBLIC = 16384
_WORKFLOW = common.REPOSITORY + "/.github/workflows/compat.yml@"


def need(value, code="cli-input-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode("ascii")


@dataclass(frozen=True)
class Context:
    sha: str
    run_id: int
    attempt: int
    target: str | None

    @property
    def suffix(self):
        return f"{self.sha}-{self.run_id}-{self.attempt}"


def context(env):
    need(env.get("GITHUB_REPOSITORY") == common.REPOSITORY, "cli-repository-invalid")
    need(env.get("RUNNER_ENVIRONMENT") == "github-hosted" and env.get("RUNNER_DEBUG") != "1",
         "cli-runner-invalid")
    sha, ref, event = (env.get(k) for k in ("GITHUB_SHA", "GITHUB_REF", "GITHUB_EVENT_NAME"))
    need(type(sha) is str and _SHA.fullmatch(sha), "cli-source-invalid")
    need(type(ref) is str, "cli-ref-invalid")
    if event == "push":
        need(ref == "refs/heads/main", "cli-ref-invalid")
    elif event == "pull_request":
        need(re.fullmatch(r"refs/pull/[1-9][0-9]{0,9}/merge", ref), "cli-ref-invalid")
    elif event == "workflow_dispatch":
        need(re.fullmatch(r"refs/heads/[A-Za-z0-9_./-]{1,200}", ref)
             and ".." not in ref and not ref.endswith("/"), "cli-ref-invalid")
    else:
        raise proof.EvidenceError("cli-event-invalid")
    need(env.get("GITHUB_WORKFLOW_REF") == _WORKFLOW + ref
         and env.get("GITHUB_WORKFLOW_SHA") == sha, "cli-workflow-invalid")
    run, attempt = env.get("GITHUB_RUN_ID"), env.get("GITHUB_RUN_ATTEMPT")
    need(type(run) is str and re.fullmatch(r"[1-9][0-9]{0,15}", run)
         and int(run) < 10**16, "cli-run-invalid")
    need(type(attempt) is str and re.fullmatch(r"[1-9][0-9]{0,3}", attempt), "cli-attempt-invalid")
    target = env.get("COMPAT_ROLLBACK_TARGET", "")
    need(type(target) is str and (not target or event == "workflow_dispatch"), "cli-target-invalid")
    if target:
        proof.validate_rollback_target(target)
    return Context(sha, int(run), int(attempt), target or None)


def regular_path(path, *, directory=False):
    need(isinstance(path, Path) and path.is_absolute() and path.resolve() == path,
         "cli-path-invalid")
    need(not any(p.is_symlink() or getattr(p, "is_junction", lambda: False)()
                 for p in (path, *path.parents)), "cli-path-invalid")
    need(path.is_dir() if directory else path.is_file(), "cli-path-invalid")
    return path


def _identity(entry):
    return {"release": entry.release, "sha": entry.sha, "web_digest": entry.images["web"]}


def validate_snapshot(value, ctx):
    need(type(value) is dict and set(value) == {"version", "sha", "run_id", "run_attempt",
         "requested_rollback_target", "latest", "selected", "preview"}, "cli-snapshot-invalid")
    need(type(value["version"]) is int and value["version"] == 1
         and value["sha"] == ctx.sha and type(value["run_id"]) is int and value["run_id"] == ctx.run_id
         and type(value["run_attempt"]) is int and value["run_attempt"] == ctx.attempt
         and value["requested_rollback_target"] == ctx.target, "cli-snapshot-context")
    for name in ("latest", "selected"):
        item = value[name]
        need(type(item) is dict and set(item) == {"release", "sha", "web_digest"}, "cli-snapshot-identity")
        proof.validate_rollback_target(item["release"])
        need(type(item["sha"]) is str and _SHA.fullmatch(item["sha"])
             and type(item["web_digest"]) is str and common.DIGEST_RE.fullmatch(item["web_digest"]),
             "cli-snapshot-identity")
    if ctx.target is None:
        need(value["selected"] == value["latest"], "cli-snapshot-selection")
    else:
        need(value["selected"]["release"] == ctx.target, "cli-snapshot-selection")
    preview = value["preview"]
    need(type(preview) is dict and set(preview) == {"kind", "dormant", "blocked"}
         and preview["kind"] in {"none", "data"}
         and type(preview["dormant"]) is bool and type(preview["blocked"]) is bool
         and preview["dormant"] == (preview["kind"] == "data"), "cli-snapshot-preview")
    return value


def preview(repo, ctx, client):
    """Use the existing trust algorithms once; do not duplicate signer policy."""
    deadline = images._Deadline()
    deadline.end = min(deadline.end, deadline.now() + 240)
    with images._verification_transport(client, deadline):
        source = images._Source(repo, ctx.sha, deadline)
        source.clean()
        bootstrap = records.load_bootstrap()
        chain = records.resolve_chain(client, repo, bootstrap=bootstrap, work_dir=client.work_dir)
        index = None if ctx.target is None else chain.find(ctx.target)
        need(ctx.target is None or index is not None, "cli-target-missing")
        latest = chain.latest
        selected = latest if index is None else chain.entries[index]
        records.verify_images(client, latest.images, latest.sha, bootstrap)
        if selected.release != latest.release:
            records.verify_images(client, selected.images, selected.sha, bootstrap)
        classification = schema_change.classify(repo, latest.sha, ctx.sha, mode="forward")
        source.clean()
    value = {"version": 1, "sha": ctx.sha, "run_id": ctx.run_id, "run_attempt": ctx.attempt,
             "requested_rollback_target": ctx.target, "latest": _identity(latest), "selected": _identity(selected),
             "preview": {"kind": classification.kind, "dormant": classification.dormant,
                         "blocked": bool(classification.reasons)}}
    return validate_snapshot(value, ctx)


def validate_result(raw, snapshot, ctx):
    validate_snapshot(snapshot, ctx)
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_PUBLIC, "cli-result-invalid")
    value = common.loads_strict(raw)
    need(proof.result_bytes(value) == raw, "cli-result-noncanonical")
    selected = snapshot["selected"]
    need(value["sha"] == ctx.sha and value["run_id"] == ctx.run_id and value["run_attempt"] == ctx.attempt
         and value["previous_release"] == selected["release"]
         and value["previous_web_digest"] == selected["web_digest"], "cli-result-context")
    need(snapshot["preview"]["blocked"] is False, "cli-preview-blocked")
    return value


def accepted(value):
    proof.validate_result(value)
    return value["forward"] and value["reversible"] and all(value["old_migrate_ok"].values()) \
        and all(value["old_server_ok"].values())


def execute(repo, ctx, client, snapshot, *, docker_binary, config_dir, image_work_dir, buildx_binary):
    validate_snapshot(snapshot, ctx)
    need(snapshot["preview"]["blocked"] is False, "cli-preview-blocked")
    selected = snapshot["selected"]
    raw = compat.run_experiment(repo, ctx.sha, ctx.run_id, ctx.attempt, client,
        docker_binary=docker_binary, config_dir=config_dir, image_work_dir=image_work_dir,
        buildx_binary=buildx_binary, rollback_target=selected["release"], expected_previous_sha=selected["sha"])
    validate_result(raw, snapshot, ctx)
    return raw  # An actual false result is valid evidence, not a passing gate.


def _read_public(path):
    regular_path(path)
    with path.open("rb") as stream:
        raw = stream.read(_MAX_PUBLIC + 1)
    need(0 < len(raw) <= _MAX_PUBLIC, "cli-public-bound")
    return raw


def _write_public(path, raw):
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_PUBLIC, "cli-public-bound")
    regular_path(path.parent, directory=True)
    need(not path.exists() and not path.is_symlink(), "cli-public-exists")
    # A partial write never qualifies: caller/aggregator demands canonical
    # complete bytes and this process must succeed before upload is permitted.
    with path.open("xb") as stream:
        stream.write(raw)


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


def _client(work, env):
    token = env.get("GH_TOKEN")
    need(type(token) is str and 0 < len(token) <= 8192 and not any(c in token for c in "\r\n\0"),
         "cli-token-unavailable")
    binary = regular_path(Path(env.get("COMPAT_GH_BIN", "")))
    def version_capture(argv, **options):
        need(argv == [str(binary), "--version"], "cli-constructor-command-invalid")
        options["timeout"] = min(options["timeout"], 5)
        return runtime.bounded_command(argv, **options)
    client = runtime.ReadOnlyGh(str(binary), token, expected_version="2.98.0",
        config_dir=work / "gh-config", work_dir=work / "gh-work",
        base_env=common.scrubbed_env(base=env), capture=version_capture)
    # Construction is bounded setup, before the experiment's shared clock.
    # Its verification scope requires the normal untouched transport identity.
    client.capture = runtime.bounded_command
    return client


def main(argv=None):
    try:
        args = sys.argv[1:] if argv is None else argv
        need(type(args) is list and len(args) == 1 and type(args[0]) is str
             and args[0] in {"preview", "run", "check"}, "cli-command-invalid")
        command = args[0]
        need(images._READY_FOR_EXECUTION, "cli-experiment-disabled")
        ctx = context(os.environ)
        repo = regular_path(Path.cwd().resolve(), directory=True)
        root = regular_path(Path(os.environ.get("RUNNER_TEMP", "")), directory=True)
        if command == "preview":
            work = _scratch(repo, ctx, "preview", os.environ)
            value = preview(repo, ctx, _client(work, os.environ))
            _write_public(work / "public" / "resolution.json", canonical(value))
            return 1 if value["preview"]["blocked"] else 0
        raw = _read_public(root / "compat-input" / "resolution.json")
        snapshot = common.loads_strict(raw)
        need(canonical(validate_snapshot(snapshot, ctx)) == raw, "cli-snapshot-noncanonical")
        if command == "check":
            need(os.environ.get("COMPAT_PREVIEW_OUTCOME") == "success"
                 and os.environ.get("COMPAT_RUN_OUTCOME") == "success", "cli-upstream-failed")
            result = _read_public(root / "compat-input" / "compat-result.json")
            value = validate_result(result, snapshot, ctx)
            work = _scratch(repo, ctx, "check", os.environ)
            _write_public(work / "public" / "compat-result.json", result)
            _mark_complete(os.environ)
            return 0 if accepted(value) else 1
        work = _scratch(repo, ctx, "run", os.environ)
        docker = regular_path(Path(os.environ.get("COMPAT_DOCKER_BIN", "")))
        plugin = regular_path(Path(os.environ.get("COMPAT_BUILDX_BIN", "")))
        result = execute(repo, ctx, _client(work, os.environ), snapshot, docker_binary=str(docker),
            config_dir=work / "docker-config", image_work_dir=work / "images", buildx_binary=plugin)
        _write_public(work / "public" / "compat-result.json", result)
        return 0
    except (proof.EvidenceError, common.ReleaseError, OSError, ValueError, TypeError, KeyError):
        # No exception strings, paths, headers, tokens, SQL or subprocess bytes.
        print("compatibility-entrypoint-refused", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
