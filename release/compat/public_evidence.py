"""Shared public compatibility wire validators without experiment execution.

These fixed parsers validate local public bytes; they do not authenticate source,
qualify history, publish intent or authorize release actions. regular_path keeps
its explicit caller-requested local path checks. Import performs no discovery.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import hashlib
import json
import re

import proof
import ship_common as common
import compat_evidence
import pending_binding

_SHA = re.compile(r"[0-9a-f]{40}\Z")
_MAX_PUBLIC = 16384
_MAX_CONTEXT = 4096
_WORKFLOW_PATH = ".github/workflows/compat.yml"
_WORKFLOW = common.REPOSITORY + "/.github/workflows/compat.yml@"

PENDING_ACTIVE_ARTIFACTS = (
    ('active_catalog', '17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8'),
    ('active_globals', 'c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3'),
    ('active_history', '9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513'),
)


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
    event: str
    ref: str

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
    return Context(sha, int(run), int(attempt), target or None, event, ref)


def regular_path(path, *, directory=False):
    need(isinstance(path, Path) and path.is_absolute() and path.resolve() == path,
         "cli-path-invalid")
    need(not any(p.is_symlink() or getattr(p, "is_junction", lambda: False)()
                 for p in (path, *path.parents)), "cli-path-invalid")
    need(path.is_dir() if directory else path.is_file(), "cli-path-invalid")
    return path


def validate_snapshot(value, ctx):
    need(type(value) is dict and set(value) == {"version", "sha", "run_id", "run_attempt",
         "requested_rollback_target", "latest", "selected", "preview", "database_base",
         "selected_web_source_sha", "pending"}, "cli-snapshot-invalid")
    need(type(value["version"]) is int and value["version"] == 4
         and value["sha"] == ctx.sha and type(value["run_id"]) is int and value["run_id"] == ctx.run_id
         and type(value["run_attempt"]) is int and value["run_attempt"] == ctx.attempt
         and value["requested_rollback_target"] == ctx.target, "cli-snapshot-context")
    for name in ("latest", "selected"):
        item = value[name]
        need(type(item) is dict and set(item) == {"release", "manifest_sha256", "sha", "web_digest"},
             "cli-snapshot-identity")
        proof.validate_rollback_target(item["release"])
        need(type(item["sha"]) is str and _SHA.fullmatch(item["sha"])
             and type(item["manifest_sha256"]) is str
             and re.fullmatch(r"[0-9a-f]{64}", item["manifest_sha256"])
             and type(item["web_digest"]) is str and common.DIGEST_RE.fullmatch(item["web_digest"]),
             "cli-snapshot-identity")
    if ctx.target is None:
        need(value["selected"] == value["latest"], "cli-snapshot-selection")
    else:
        need(value["selected"]["release"] == ctx.target, "cli-snapshot-selection")
    try:
        compat_evidence._database_base_dict(value["database_base"],
            compat_evidence.ReleaseIdentity(**value["latest"]))
    except compat_evidence.CompatRefusal:
        raise proof.EvidenceError("cli-snapshot-base") from None
    need(type(value["selected_web_source_sha"]) is str and _SHA.fullmatch(value["selected_web_source_sha"]),
         "cli-snapshot-source")
    try:
        binding = pending_binding.validate_against(pending_binding.parse_wire(value["pending"]),
            candidate_sha=ctx.sha, previous_sha=value["latest"]["sha"],
            active_artifacts=PENDING_ACTIVE_ARTIFACTS)
    except pending_binding.PendingBindingRefusal:
        raise proof.EvidenceError("cli-snapshot-pending") from None
    preview = value["preview"]
    need(type(preview) is dict and set(preview) == {"kind", "dormant", "blocked"}
         and preview["kind"] in {"none", "data", "expand"}
         and type(preview["dormant"]) is bool and type(preview["blocked"]) is bool
         and preview["dormant"] == (preview["kind"] != "none")
         and (preview["kind"] != "expand" or binding.target_version is not None), "cli-snapshot-preview")
    return value


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
    if ctx.target is None:
        need(value["classification"] == {k: snapshot["preview"][k] for k in ("kind", "dormant")},
             "cli-result-classification")
    return value


def evidence_context(result, snapshot, ctx):
    """Bind complete measured bytes to the authenticated frozen selection.

    This is not a success verdict or a claim of current provider state.
    Its digest covers the unchanged canonical result schema, including LF.
    """
    validate_result(result, snapshot, ctx)
    need(ctx.target is None, "cli-target-proof-unsupported")
    return {"version": 3, "repository": common.REPOSITORY, "workflow_path": _WORKFLOW_PATH,
            "workflow_sha": ctx.sha, "event": ctx.event, "ref": ctx.ref, "sha": ctx.sha,
            "run_id": ctx.run_id, "run_attempt": ctx.attempt,
            "requested_rollback_target": ctx.target, "latest": snapshot["latest"],
            "selected": snapshot["selected"], "preview": snapshot["preview"],
            "database_base": snapshot["database_base"],
            "selected_web_source_sha": snapshot["selected_web_source_sha"],
            "pending": snapshot["pending"],
            "result_sha256": hashlib.sha256(result).hexdigest()}


def validate_evidence(raw, result, snapshot, ctx):
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_CONTEXT, "cli-context-bound")
    value = common.loads_strict(raw)
    # Byte equality enforces every closed field/type, including bool vs int,
    # and rebinds the context to this job rather than trusting uploaded fields.
    need(canonical(value) == raw and raw == canonical(evidence_context(result, snapshot, ctx)),
         "cli-evidence-context")
    return value


def accepted(value):
    proof.validate_result(value)
    return value["forward"] and value["reversible"] and all(value["old_migrate_ok"].values()) \
        and all(value["old_server_ok"].values())


def _rollback_context(ctx):
    """Explicit main-dispatch rollback identity; normal routing stays separate."""
    need(type(ctx) is Context and type(ctx.sha) is str and _SHA.fullmatch(ctx.sha)
         and type(ctx.run_id) is int and 1 <= ctx.run_id < 10**16
         and type(ctx.attempt) is int and 1 <= ctx.attempt <= 9999,
         "cli-snapshot-context")
    need(ctx.event == "workflow_dispatch" and ctx.ref == "refs/heads/main"
         and ctx.target is not None, "cli-target-invalid")
    proof.validate_rollback_target(ctx.target)


def validate_rollback_snapshot(value, ctx):
    _rollback_context(ctx)
    need(type(value) is dict and set(value) == {"version", "purpose", "sha", "run_id", "run_attempt",
         "requested_rollback_target", "latest", "selected", "preview", "database_base",
         "selected_web_source_sha", "live_web_source_sha", "span", "source_material_sha256"},
         "cli-snapshot-invalid")
    need(type(value["version"]) is int and value["version"] == 1 and value["purpose"] == "rollback-v0"
         and value["sha"] == ctx.sha and type(value["run_id"]) is int and value["run_id"] == ctx.run_id
         and type(value["run_attempt"]) is int and value["run_attempt"] == ctx.attempt
         and value["requested_rollback_target"] == ctx.target, "cli-snapshot-context")
    try:
        binding = compat_evidence.parse_rollback_binding({"producer_sha": value["sha"],
            **{key: value[key] for key in ("latest", "selected", "database_base", "selected_web_source_sha",
                                          "live_web_source_sha", "span", "source_material_sha256")}})
    except compat_evidence.CompatRefusal:
        raise proof.EvidenceError("cli-snapshot-invalid") from None
    need(binding.selected.release == ctx.target, "cli-snapshot-selection")
    preview = value["preview"]
    need(type(preview) is dict and set(preview) == {"kind", "dormant", "blocked"}
         and preview["kind"] == "none" and preview["dormant"] is False and preview["blocked"] is False,
         "cli-snapshot-preview")
    return value


def validate_rollback_result(raw, snapshot, ctx):
    validate_rollback_snapshot(snapshot, ctx)
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_PUBLIC, "cli-result-invalid")
    value = common.loads_strict(raw)
    need(proof.result_bytes(value) == raw, "cli-result-noncanonical")
    selected = snapshot["selected"]
    need(value["sha"] == ctx.sha and value["run_id"] == ctx.run_id and value["run_attempt"] == ctx.attempt
         and value["previous_release"] == selected["release"]
         and value["previous_web_digest"] == selected["web_digest"], "cli-result-context")
    need(value["classification"] == {"kind": "none", "dormant": False}, "cli-result-classification")
    # Preserve the actual final catalog, including a complete mismatch. The
    # later qualifier compares it with the retained source expectation.
    return value


def rollback_evidence_context(result, snapshot, ctx):
    """Bind complete actual rollback measurements, including false flags.

    The unchanged public result identifies producer H and target T. Its
    context records signed live L and retained origin O separately; it never
    asserts history qualification or authorizes an operational rollback.
    """
    validate_rollback_result(result, snapshot, ctx)
    value = {**snapshot, "version": 4, "repository": common.REPOSITORY,
        "workflow_path": _WORKFLOW_PATH, "workflow_sha": ctx.sha, "event": ctx.event, "ref": ctx.ref,
        "result_sha256": hashlib.sha256(result).hexdigest()}
    need(len(canonical(value)) <= _MAX_CONTEXT, "cli-context-bound")
    return value


def validate_rollback_evidence(raw, result, snapshot, ctx):
    need(type(raw) is bytes and 0 < len(raw) <= _MAX_CONTEXT, "cli-context-bound")
    value = common.loads_strict(raw)
    need(canonical(value) == raw and raw == canonical(rollback_evidence_context(result, snapshot, ctx)),
         "cli-evidence-context")
    return value
