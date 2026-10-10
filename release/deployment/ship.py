#!/usr/bin/env python3
"""CLI orchestrator for the Release workflow (.github/workflows/ship.yml).

Subcommands, one per workflow step:

  plan                                  before approval, no secrets
  image --stage pins|contract|trivy-db|record --component C
  trivy-policy
  candidate --stage read|verify|assemble
  production                            the only step that sees the DO token
  record --stage manifest|publish|verify

Exit codes: 0 success; 1 refused, production unchanged; 2 failed after the
PUT and the automatic rollback restored the pre-PUT spec with health passing;
3 MANUAL INTERVENTION (state uncertain or rollback failed): follow
docs/emergency-rollback.md.

Everything printed or written to GITHUB_OUTPUT / GITHUB_STEP_SUMMARY goes
through ship_common.Output. Exceptions are reported by reason code only.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import os
import re
import signal
import stat
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, Callable, Mapping, MutableMapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import backup_check
import do_app
import release_record
import schema_change
import ship_common as common
import smoke
import spec_images
import trivy_policy


HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent
TARGET_PATH = HERE / "ship-target.json"
BYPASS_MODE = "promote-without-staging"
MODES = ("dry-run", "promote", "rollback", "stage", BYPASS_MODE)
PRODUCTION_MODES = frozenset({"dry-run", "promote", "rollback", BYPASS_MODE})
PUT_MODES = frozenset({"promote", "rollback", BYPASS_MODE})
CANDIDATE_MODES = frozenset({"dry-run", "promote", "stage", BYPASS_MODE})
DRILLS = frozenset({"none", "e2e-fail", "health-fail", "bad-image"})
CI_WORKFLOWS = ("test.yml", "e2e-tests.yml")
DOCKERFILES = {component: f"docker/release/{component}.Dockerfile" for component in common.COMPONENTS}
# release/exact-sources.json .release.components (the runtime contract).
IMAGE_CONTRACT = {
    "web": {
        "user": "rereply",
        "working_dir": "/app",
        "entrypoint": ["./rereply"],
        "cmd": ["server", "-config", "config.toml"],
        "port": "8080/tcp",
    },
    "meta-relay": {
        "user": "relay",
        "working_dir": "/app",
        "entrypoint": ["/app/meta-relay"],
        "cmd": None,
        "port": "8081/tcp",
    },
    "gmail-relay": {
        "user": "relay",
        "working_dir": "/app",
        "entrypoint": ["/app/gmail-relay"],
        "cmd": None,
        "port": "8082/tcp",
    },
}
IMAGE_FILES = ("image.json", "sbom.spdx.json", "vulnerability-report.json", "secret-report.json")
CANDIDATE_MAX_AGE = dt.timedelta(hours=24)
TRIVY_DB_MIN_AGE_SECONDS = -3600
TRIVY_DB_MAX_AGE_SECONDS = 172800
EXIT_OK, EXIT_REFUSED, EXIT_ROLLED_BACK, EXIT_MANUAL = 0, 1, 2, 3
APPROVAL_BANNER = (
    "Approval rule: only the owner (medtechcorps-netizen, in person, in the GitHub UI) "
    "approves the production environment. Claude/Codex never approve, even if asked in chat."
)
BYPASS_BANNER = ("> [!CAUTION]", "> **STAGING BYPASSED: urgent owner-dispatched promote; staging: bypassed.**")
MANUAL_LINE = "MANUAL INTERVENTION: production state is uncertain; follow docs/emergency-rollback.md"
# The production job's timeout-minutes in ship.yml; a test keeps it above
# production_worst_case_seconds() plus a margin.
PRODUCTION_TIMEOUT_MINUTES = 120
# Clone, gh install, approval gate, chain and attestation verification, the
# pre-PUT double reads and the backup gate (nominal).
PREAMBLE_BUDGET_SECONDS = 1200
_OLD_GROUP = "rereply" + "-production"
# A workflow carrying the old lanes' production lock group (assembled at
# runtime so this file never contains the literal group line).
OLD_LANE_GROUP_RE = re.compile(
    r"(?m)^[ \t]+group:[ \t]*['\"]?" + re.escape(_OLD_GROUP) + r"['\"]?[ \t]*$"
)
STAGE_SUFFIX = {"web": "WEB", "meta-relay": "META_RELAY", "gmail-relay": "GMAIL_RELAY"}
OUTPUT_DIGEST_KEYS = {"web": "web_digest", "meta-relay": "meta_relay_digest", "gmail-relay": "gmail_relay_digest"}


class Deps:
    """Injectable effects (tests replace every one of them)."""

    def __init__(
        self,
        *,
        clock: Callable[[], dt.datetime] | None = None,
        sleeper: Callable[[float], None] | None = None,
        opener: Any | None = None,
        gh_runner: Callable[..., Any] | None = None,
        https_request: Callable[..., Any] | None = None,
        stdout: Any | None = None,
        repo_dir: Path | None = None,
        target_path: Path | None = None,
        staging_pins_path: Path | None = None,
        bootstrap_path: Path | None = None,
        bootstrap_sha256: str | None = None,
        trivy_policy_path: Path | None = None,
        work_dir: Path | None = None,
        poll_limit: int | None = None,
        smoke_rounds: int | None = None,
        smoke_delay: float | None = None,
    ) -> None:
        self.clock = clock or common.utc_now
        self.sleeper = sleeper or time.sleep
        self.opener = opener
        self.gh_runner = gh_runner or subprocess.run
        self.https_request = https_request or smoke.secure_https_request
        self.stdout = stdout
        self.repo_dir = repo_dir or REPO_ROOT
        self.target_path = target_path or TARGET_PATH
        self.staging_pins_path = staging_pins_path or HERE / "ship-target-staging.json"
        self.bootstrap_path = bootstrap_path or release_record.BOOTSTRAP_PATH
        self.bootstrap_sha256 = bootstrap_sha256 or release_record.BOOTSTRAP_SHA256
        self.trivy_policy_path = trivy_policy_path or trivy_policy.POLICY_PATH
        self.work_dir = work_dir
        self.poll_limit = poll_limit or do_app.POLL_LIMIT
        self.smoke_rounds = smoke_rounds or smoke.ROUNDS
        self.smoke_delay = smoke.ROUND_DELAY_SECONDS if smoke_delay is None else smoke_delay


class Context:
    def __init__(self, env: MutableMapping[str, str], deps: Deps, out: common.Output) -> None:
        self.env = env
        self.deps = deps
        self.out = out
        self._gh: release_record.Gh | None = None

    @property
    def repo_dir(self) -> Path:
        return self.deps.repo_dir

    def now(self) -> dt.datetime:
        return common.checked_clock(self.deps.clock())

    def runner_temp(self) -> Path:
        raw = self.env.get("RUNNER_TEMP")
        if type(raw) is not str or not raw:
            common.fail("context-invalid:runner-temp")
        path = Path(raw)
        if path.is_symlink() or not path.is_dir():
            common.fail("context-invalid:runner-temp")
        return path

    def work_dir(self, name: str) -> Path:
        base = self.deps.work_dir or (self.runner_temp() / "ship-work")
        path = base / name
        path.mkdir(parents=True, exist_ok=True)
        return path

    def gh(self) -> release_record.Gh:
        if self._gh is None:
            self._gh = release_record.Gh(
                self.env.get("SHIP_GH_BIN", ""),
                self.env.get("GH_TOKEN", ""),
                common.REPOSITORY,
                expected_version=self.env.get("PINNED_GH_VERSION", ""),
                runner=self.deps.gh_runner,
                base_env=self.env,
                config_dir=str(self.work_dir("gh-config")),
            )
        return self._gh

    def bootstrap(self) -> dict[str, Any]:
        return release_record.load_bootstrap(self.deps.bootstrap_path, self.deps.bootstrap_sha256)

    def ship_target(self) -> dict[str, Any]:
        return spec_images.validate_target(common.load_json(self.deps.target_path, "target-invalid:ship-target"))

    def health(self, origin: str) -> dict[str, str]:
        return smoke.run_health(
            origin,
            request=self.deps.https_request,
            rounds=self.deps.smoke_rounds,
            delay=self.deps.smoke_delay,
            sleeper=self.deps.sleeper,
        )


# --------------------------------------------------------------------------
# Shared gates
# --------------------------------------------------------------------------


def parse_mode(env: Mapping[str, str]) -> tuple[str, str]:
    mode = env.get("SHIP_MODE")
    if mode not in MODES:
        common.fail("input-invalid:mode")
    if mode == BYPASS_MODE and env.get("GITHUB_ACTOR") != common.APPROVER_LOGIN:
        common.fail("input-invalid:staging-bypass-owner")
    drill = env.get("SHIP_DRILL", "none")
    if drill not in DRILLS or (mode in PRODUCTION_MODES and drill != "none"):
        common.fail("input-invalid:stage-drill")
    target = env.get("SHIP_TARGET_RELEASE", "")
    if type(target) is not str:
        common.fail("input-invalid:target-release")
    if mode == "rollback":
        if target != common.BOOTSTRAP_TAG and common.RECORD_TAG_RE.fullmatch(target) is None:
            common.fail("input-invalid:target-release")
    elif target != "":
        common.fail("input-invalid:target-release")
    return mode, target


def require_ci_green(gh: release_record.Gh, sha: str) -> None:
    """Test and E2E push runs on main for this exact SHA concluded success."""
    for workflow in CI_WORKFLOWS:
        runs = gh.workflow_runs(workflow, sha)
        if not any(
            type(run) is dict
            and run.get("status") == "completed"
            and run.get("conclusion") == "success"
            and run.get("head_sha") == sha
            and run.get("path") == f".github/workflows/{workflow}"
            and run.get("event") == "push"
            and run.get("head_branch") == "main"
            for run in runs
        ):
            common.fail("ci-not-green")


def old_lane_paths(repo_dir: Path) -> set[str]:
    directory = repo_dir / ".github" / "workflows"
    paths: set[str] = set()
    if not directory.is_dir():
        return paths
    for path in sorted(directory.iterdir()):
        if path.suffix not in {".yml", ".yaml"} or not path.is_file():
            continue
        if OLD_LANE_GROUP_RE.search(path.read_text(encoding="utf-8", errors="replace")):
            paths.add(f".github/workflows/{path.name}")
    return paths


def require_old_lanes_idle(gh: release_record.Gh, repo_dir: Path) -> None:
    lanes = old_lane_paths(repo_dir)
    for run in gh.active_runs():
        if type(run) is not dict or type(run.get("path")) is not str or type(run.get("id")) is not int:
            common.fail("gh-failed:active-runs")
        if run["path"].split("@", 1)[0] in lanes:
            common.fail("old-lane-active")


def first_parent_commits(out: common.Output, repo_dir: Path, base: str, head: str) -> tuple[int, list[str]]:
    raw = release_record.git_output(
        repo_dir, ["log", "--first-parent", "--format=%h %s", f"{base}..{head}"], code="git-failed:log"
    )
    lines = [line for line in raw.decode("utf-8", "replace").split("\n") if line]
    shown: list[str] = []
    for line in lines[:50]:
        short, _, subject = line.partition(" ")
        subject = "".join(ch if 0x20 <= ord(ch) <= 0x7E else "?" for ch in subject)[:100]
        text = f"- {short} {subject}"
        try:
            common.require_public_text(text, out.private)
        except common.ReleaseError:
            text = f"- {short} (subject withheld)"
        shown.append(text)
    return len(lines), shown


def report_schema_block(out: common.Output, exc: schema_change.SchemaChangeBlocked) -> None:
    lines = ["refused: schema-change-blocked (interim schema freeze; Stage 2 lifts it)"]
    for reason in exc.reasons[:50]:
        try:
            lines.append("  " + common.require_public_text(reason, out.private))
        except common.ReleaseError:
            lines.append("  (path withheld)")
    for line in lines:
        safe_text(out, line)
    try:
        out.summary(["### Release refused", "", *lines])
    except (common.ReleaseError, OSError):
        pass


def safe_text(out: common.Output, line: str) -> None:
    try:
        out.text(line)
    except common.ReleaseError:
        out.text("refused: output-unsafe")


def _digest_lines(images: Mapping[str, str]) -> list[str]:
    return [f"- {component}: {images[component]}" for component in common.COMPONENTS]


# --------------------------------------------------------------------------
# plan
# --------------------------------------------------------------------------


def cmd_plan(ctx: Context) -> int:
    out = ctx.out
    control = common.validate_context(ctx.env)
    sha = control["sha"]
    mode, target_release = parse_mode(ctx.env)
    active_exceptions = None
    if mode in CANDIDATE_MODES:
        active_exceptions = trivy_policy.check(ctx.deps.trivy_policy_path, ctx.now().date())
    gh = ctx.gh()
    # The owner-only approval rule must be enforced by the environment
    # before anyone is asked to approve (production re-checks it).
    if mode in PRODUCTION_MODES:
        release_record.require_approval_gate(gh)
    if mode in CANDIDATE_MODES:
        require_ci_green(gh, sha)
    require_old_lanes_idle(gh, ctx.repo_dir)
    bootstrap = ctx.bootstrap()
    chain = release_record.resolve_chain(gh, ctx.repo_dir, bootstrap=bootstrap, work_dir=ctx.work_dir("plan-chain"))
    latest = chain.latest
    lines = [
        "## Release plan",
        "",
        f"- Mode: {mode}",
        f"- Commit: {sha}",
        f"- Latest record: {latest.release} ({latest.kind}, commit {latest.sha})",
    ]
    if mode in PRODUCTION_MODES:
        lines.append("- Approval gate: environment production requires the owner's review (admin bypass off, main only)")
    else:
        lines.append("- Staging only: separate staging environments; no production deployment or record")
    outputs = {
        "latest_release": latest.release,
        "latest_manifest_sha256": latest.manifest_sha256,
        "latest_sha": latest.sha,
        "target_release": "",
        "target_manifest_sha256": "",
    }
    if mode in CANDIDATE_MODES:
        release_record.require_no_downgrade(ctx.repo_dir, latest.sha, sha)
        schema_change.guard(ctx.repo_dir, latest.sha, sha)
        count, commits = first_parent_commits(out, ctx.repo_dir, latest.sha, sha)
        lines += [
            "- No-downgrade: the commit descends from the latest record",
            "- Schema guard: clean",
            "- CI: Test and E2E push runs on main succeeded for this commit",
            "- Old release lanes: idle",
            f"- Active Trivy exceptions: {active_exceptions}",
            "",
            f"### First-parent commits since {latest.release} ({count})",
            "",
            *commits,
        ]
        if count > len(commits):
            lines.append(f"- ... and {count - len(commits)} more")
        if mode in PRODUCTION_MODES:
            lines += ["", "The production job deploys only if live digests equal the latest record."]
        else:
            lines += ["", "Staging verifies the configured target, deployment receipt and all thirteen canary checks."]
    else:
        target, intermediates = release_record.rollback_plan(chain, target_release)
        schema_change.guard(ctx.repo_dir, target.sha, latest.sha)
        outputs["target_release"] = target.release
        outputs["target_manifest_sha256"] = target.manifest_sha256
        lines += [
            f"- Rollback target: {target.release} (commit {target.sha})",
            *_digest_lines(target.images),
            f"- Records after the target: {len(intermediates)} (none changed the database)",
            "- Schema guard between target and latest: clean",
            "- Old release lanes: idle",
            "",
            "The production job reads live digests and decides: rollback (live equals the latest "
            "record; one PUT to the target), reconcile (live already equals the target; no PUT, "
            "record only) or restore (target is the latest record and live matches no record; one "
            "PUT back to the latest record).",
        ]
    if mode in PRODUCTION_MODES:
        lines += ["", APPROVAL_BANNER]
    if mode == BYPASS_MODE:
        lines += ["", *BYPASS_BANNER]
    elif mode == "promote":
        lines += ["", "Production additionally requires this run's bound thirteen-check staging report."]
    out.set_outputs(outputs)
    out.summary(lines)
    out.text(f"plan ok: {mode} at {sha}; latest record {latest.release}")
    if mode in PRODUCTION_MODES:
        out.text(APPROVAL_BANNER)
    if mode == BYPASS_MODE:
        for line in BYPASS_BANNER:
            out.text(line)
    return EXIT_OK


# --------------------------------------------------------------------------
# images job
# --------------------------------------------------------------------------


def _require_component(component: Any) -> str:
    if component not in common.COMPONENTS:
        common.fail("input-invalid:component")
    return component


def _logical_lines(text: str) -> list[str]:
    lines: list[str] = []
    current = ""
    for raw in text.split("\n"):
        stripped = raw.strip()
        if not current and (not stripped or stripped.startswith("#")):
            continue
        if raw.rstrip().endswith("\\"):
            current += raw.rstrip()[:-1] + " "
            continue
        current += raw
        lines.append(current.strip())
        current = ""
    if current.strip():
        lines.append(current.strip())
    return lines


PINNED_FROM_RE = re.compile(r"^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$")
CHECKSUM_RE = re.compile(r"^--checksum=sha256:[0-9a-f]{64}$")


def check_dockerfile_pins(repo_dir: Path, component: str) -> str:
    """docker/release/<c>.Dockerfile is a tracked regular 100644 blob, every
    FROM is name@sha256 (or scratch, or an earlier stage), every remote ADD
    carries --checksum, and COPY/RUN --from only names earlier stages."""
    code = "dockerfile-pin-invalid"
    relative = DOCKERFILES[component]
    path = repo_dir / relative
    if path.is_symlink() or not path.is_file():
        common.fail(code)
    listing = [item for item in release_record.git_output(
        repo_dir, ["ls-tree", "-z", "HEAD", "--", relative], code="git-failed:ls-tree"
    ).split(b"\x00") if item]
    if len(listing) != 1:
        common.fail(code)
    meta, _, name = listing[0].partition(b"\t")
    fields = meta.split(b" ")
    if name.decode("utf-8", "replace") != relative or fields[:2] != [b"100644", b"blob"] or len(fields) != 3:
        common.fail(code)
    hashed = release_record.git_output(repo_dir, ["hash-object", "--", relative], code="git-failed:hash-object").strip()
    if hashed != fields[2]:
        common.fail(code)
    raw = path.read_bytes()
    try:
        text = raw.decode("ascii")
    except UnicodeError as exc:
        raise common.ReleaseError(code) from exc
    if "\r" in text:
        common.fail(code)
    first = text.lstrip("\n").split("\n", 1)[0].replace(" ", "").lower()
    if first.startswith("#syntax=") or first.startswith("#escape=") or first.startswith("#check="):
        common.fail(code)
    stages: set[str] = set()
    saw_from = False
    for line in _logical_lines(text):
        tokens = line.split()
        instruction = tokens[0].upper()
        arguments = tokens[1:]
        if instruction == "FROM":
            saw_from = True
            flags = [item for item in arguments if item.startswith("--")]
            rest = [item for item in arguments if not item.startswith("--")]
            if any(flag != "--platform=linux/amd64" for flag in flags) or not rest:
                common.fail(code)
            image = rest[0]
            if len(rest) not in (1, 3) or (len(rest) == 3 and rest[1].lower() != "as"):
                common.fail(code)
            if not (image == "scratch" or image.lower() in stages or PINNED_FROM_RE.fullmatch(image)):
                common.fail(code)
            if len(rest) == 3:
                stages.add(rest[2].lower())
        elif instruction == "ADD":
            sources = [item for item in arguments if not item.startswith("--")]
            remote = [item for item in sources[:-1] if "://" in item or item.startswith("git@")]
            checksums = [item for item in arguments if item.startswith("--checksum")]
            if remote:
                if len(remote) != 1 or not remote[0].startswith("https://"):
                    common.fail(code)
                if len(checksums) != 1 or CHECKSUM_RE.fullmatch(checksums[0]) is None:
                    common.fail(code)
        elif instruction in {"COPY", "RUN"}:
            for item in arguments:
                if not item.startswith("--"):
                    break
                if item.startswith("--from="):
                    if item[len("--from="):].lower() not in stages:
                        common.fail(code)
                elif item.startswith("--mount="):
                    for part in item[len("--mount="):].split(","):
                        key, _, value = part.partition("=")
                        if key == "from" and value.lower() not in stages:
                            common.fail(code)
        elif instruction == "ONBUILD":
            common.fail(code)
    if not saw_from:
        common.fail(code)
    return common.sha256_bytes(raw)


def _scan_dir(ctx: Context) -> Path:
    path = ctx.runner_temp() / "ship-scan"
    if path.is_symlink() or not path.is_dir():
        common.fail("artifact-invalid:scan-dir")
    return path


def _image_identity(ctx: Context, component: str) -> tuple[str, str]:
    image = ctx.env.get("IMAGE")
    if image != common.IMAGE_REPOSITORY[component]:
        common.fail("image-contract-differs:image")
    digest = common.require_digest(ctx.env.get("DIGEST"), "image-contract-differs:digest")
    return image, digest


def check_image_contract(ctx: Context, component: str, sha: str) -> None:
    """build-attest-exact-release-images.yml:1309-1336 in Python."""
    code = "image-contract-differs"
    image, digest = _image_identity(ctx, component)
    inspect = common.load_json(_scan_dir(ctx) / "inspect.json", "image-contract-differs:inspect", canonical=False, decimals=True)
    if type(inspect) is not list or len(inspect) != 1 or type(inspect[0]) is not dict:
        common.fail(code)
    item = inspect[0]
    if item.get("Os") != "linux" or item.get("Architecture") != "amd64":
        common.fail("image-contract-differs:platform")
    repo_digests = item.get("RepoDigests")
    if type(repo_digests) is not list or f"{image}@{digest}" not in repo_digests:
        common.fail("image-contract-differs:repo-digest")
    config = item.get("Config")
    if type(config) is not dict:
        common.fail(code)
    expected = IMAGE_CONTRACT[component]
    ports = config.get("ExposedPorts")
    if (
        config.get("User") != expected["user"]
        or config.get("WorkingDir") != expected["working_dir"]
        or config.get("Entrypoint") != expected["entrypoint"]
        or config.get("Cmd") != expected["cmd"]
        or type(ports) is not dict
        or expected["port"] not in ports
    ):
        common.fail("image-contract-differs:runtime")
    labels = config.get("Labels")
    if type(labels) is not dict or (
        labels.get("org.opencontainers.image.source") != "https://github.com/medtechcorps-netizen/whatomate"
        or labels.get("org.opencontainers.image.revision") != sha
        or labels.get("org.opencontainers.image.version") != f"ship-{sha}"
        or labels.get("org.opencontainers.image.title") != f"rereply-release-{component}"
        or labels.get("io.rereply.release.workflow") != "ship"
    ):
        common.fail("image-contract-differs:labels")


def parse_rfc3339(value: Any, code: str) -> dt.datetime:
    if type(value) is not str:
        common.fail(code)
    match = re.fullmatch(
        r"([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{2}:[0-9]{2}:[0-9]{2})(?:\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})",
        value,
    )
    if match is None:
        common.fail(code)
    zone = "+00:00" if match.group(3) == "Z" else match.group(3)
    try:
        parsed = dt.datetime.fromisoformat(f"{match.group(1)}T{match.group(2)}{zone}")
    except ValueError as exc:
        raise common.ReleaseError(code) from exc
    return parsed.astimezone(dt.timezone.utc)


def check_trivy_db(ctx: Context) -> dict[str, Any]:
    """The fresh (unpinned) Trivy DB must be at most 48 hours old."""
    code = "image-contract-differs:trivy-db"
    database_dir = ctx.runner_temp() / "trivy-cache" / "db"
    metadata_path = database_dir / "metadata.json"
    database_path = database_dir / "trivy.db"
    for path in (metadata_path, database_path):
        if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
            common.fail(code)
    raw = metadata_path.read_bytes()
    if len(raw) > 65536:
        common.fail(code)
    metadata = common.loads_strict(raw, decimals=True, code=code)
    if type(metadata) is not dict:
        common.fail(code)
    updated = parse_rfc3339(metadata.get("UpdatedAt", metadata.get("updatedAt")), code)
    age = (ctx.now() - updated).total_seconds()
    if not TRIVY_DB_MIN_AGE_SECONDS <= age <= TRIVY_DB_MAX_AGE_SECONDS:
        common.fail(code)
    version = ctx.env.get("PINNED_TRIVY_VERSION")
    if type(version) is not str or re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version) is None:
        common.fail(code)
    record = {
        "updated_at": common.format_timestamp(updated.replace(microsecond=0)),
        "metadata_sha256": common.sha256_bytes(raw),
        "version": version,
    }
    target = _scan_dir(ctx) / "trivy-db.json"
    if target.exists() or target.is_symlink():
        common.fail("artifact-invalid:exists")
    target.write_bytes(common.canonical_file_bytes(record))
    return record


def _require_reports(scan: Path) -> tuple[dict[str, Any], bytes, bytes, bytes]:
    code = "artifact-invalid:reports"
    sbom_raw = (scan / "sbom.spdx.json").read_bytes() if (scan / "sbom.spdx.json").is_file() else b""
    vuln_raw = (scan / "vulnerability-report.json").read_bytes() if (scan / "vulnerability-report.json").is_file() else b""
    secret_raw = (scan / "secret-report.json").read_bytes() if (scan / "secret-report.json").is_file() else b""
    for name in ("sbom.spdx.json", "vulnerability-report.json", "secret-report.json"):
        if (scan / name).is_symlink():
            common.fail(code)
    if not sbom_raw or not vuln_raw or not secret_raw or len(sbom_raw) > 256 * 1024 * 1024:
        common.fail(code)
    sbom = common.loads_strict(sbom_raw, decimals=True, code=code)
    if (
        type(sbom) is not dict
        or sbom.get("spdxVersion") != "SPDX-2.3"
        or type(sbom.get("documentNamespace")) is not str
        or not sbom["documentNamespace"]
        or type(sbom.get("packages")) is not list
        or not sbom["packages"]
    ):
        common.fail(code)
    vulnerabilities = common.loads_strict(vuln_raw, decimals=True, code=code)
    if type(vulnerabilities) is not dict or type(vulnerabilities.get("Results")) is not list:
        common.fail(code)
    for result in vulnerabilities["Results"]:
        if type(result) is not dict:
            common.fail(code)
        for finding in result.get("Vulnerabilities") or []:
            if type(finding) is not dict or finding.get("Severity") in {"HIGH", "CRITICAL"}:
                common.fail(code)
    secrets = common.loads_strict(secret_raw, decimals=True, code=code)
    if type(secrets) is not dict or type(secrets.get("Results")) is not list:
        common.fail(code)
    for result in secrets["Results"]:
        if type(result) is not dict or result.get("Secrets"):
            common.fail(code)
    return sbom, sbom_raw, vuln_raw, secret_raw


def record_image(ctx: Context, component: str, control: Mapping[str, Any]) -> dict[str, Any]:
    sha = control["sha"]
    image, digest = _image_identity(ctx, component)
    tag = ctx.env.get("TAG")
    if tag != f"ship-{sha[:12]}-r{control['run_id']}":
        common.fail("image-contract-differs:tag")
    check_image_contract(ctx, component, sha)
    dockerfile_sha256 = check_dockerfile_pins(ctx.repo_dir, component)
    scan = _scan_dir(ctx)
    _sbom, sbom_raw, vuln_raw, secret_raw = _require_reports(scan)
    trivy = common.load_json(scan / "trivy-db.json", "artifact-invalid:trivy-db")
    trivy = common.exact_keys(trivy, {"updated_at", "metadata_sha256", "version"}, "artifact-invalid:trivy-db")
    active_exceptions = trivy_policy.check(ctx.deps.trivy_policy_path, ctx.now().date())
    record = {
        "schema_version": 1,
        "component": component,
        "image": image,
        "digest": digest,
        "sha": sha,
        "tag": tag,
        "tag_is_authority": False,
        "dockerfile": DOCKERFILES[component],
        "dockerfile_sha256": dockerfile_sha256,
        "built_at": common.format_timestamp(ctx.now()),
        "sbom_sha256": common.sha256_bytes(sbom_raw),
        "trivy": {
            "version": trivy["version"],
            "db_updated_at": trivy["updated_at"],
            "db_metadata_sha256": trivy["metadata_sha256"],
            "active_exceptions": active_exceptions,
        },
    }
    validate_image_record(record, component, sha)
    output = ctx.runner_temp() / "ship-image"
    if output.exists() or output.is_symlink():
        common.fail("artifact-invalid:exists")
    output.mkdir(mode=0o700)
    (output / "image.json").write_bytes(common.canonical_file_bytes(record))
    (output / "sbom.spdx.json").write_bytes(sbom_raw)
    (output / "vulnerability-report.json").write_bytes(vuln_raw)
    (output / "secret-report.json").write_bytes(secret_raw)
    return record


def validate_image_record(value: Any, component: str, sha: str) -> dict[str, Any]:
    code = "artifact-invalid:image-record"
    record = common.exact_keys(
        value,
        {
            "schema_version", "component", "image", "digest", "sha", "tag",
            "tag_is_authority", "dockerfile", "dockerfile_sha256", "built_at",
            "sbom_sha256", "trivy",
        },
        code,
    )
    if (
        record["schema_version"] != 1
        or record["component"] != component
        or record["image"] != common.IMAGE_REPOSITORY[component]
        or record["sha"] != sha
        or record["tag_is_authority"] is not False
        or record["dockerfile"] != DOCKERFILES[component]
    ):
        common.fail(code)
    common.require_digest(record["digest"], code)
    common.exact_string(record["tag"], code, re.compile(r"ship-[0-9a-f]{12}-r[1-9][0-9]{0,14}"))
    if not record["tag"].startswith(f"ship-{sha[:12]}-r"):
        common.fail(code)
    common.require_sha256(record["dockerfile_sha256"], code)
    common.require_timestamp(record["built_at"], code)
    common.require_sha256(record["sbom_sha256"], code)
    trivy = common.exact_keys(record["trivy"], {"version", "db_updated_at", "db_metadata_sha256", "active_exceptions"}, code)
    common.exact_string(trivy["version"], code)
    common.require_timestamp(trivy["db_updated_at"], code)
    common.require_sha256(trivy["db_metadata_sha256"], code)
    common.exact_int(trivy["active_exceptions"], code, 0, trivy_policy.MAX_ENTRIES)
    return record


def cmd_image(ctx: Context, stage: str, component: str) -> int:
    component = _require_component(component)
    control = common.validate_context(ctx.env)
    if stage == "pins":
        dockerfile_sha256 = check_dockerfile_pins(ctx.repo_dir, component)
        ctx.out.text(f"{DOCKERFILES[component]}: pins reviewed (sha256 {dockerfile_sha256})")
    elif stage == "contract":
        check_image_contract(ctx, component, control["sha"])
        ctx.out.text(f"{component}: runtime contract verified")
    elif stage == "trivy-db":
        record = check_trivy_db(ctx)
        ctx.out.text(f"Trivy database updated at {record['updated_at']}")
    elif stage == "record":
        record = record_image(ctx, component, control)
        ctx.out.text(f"{component}: {record['image']}@{record['digest']} recorded")
    else:
        common.fail("input-invalid:stage")
    return EXIT_OK


def cmd_trivy_policy(ctx: Context) -> int:
    common.validate_context(ctx.env)
    count = trivy_policy.check(ctx.deps.trivy_policy_path, ctx.now().date())
    ctx.out.text(f"reviewed Trivy exceptions valid: {count} active")
    return EXIT_OK


# --------------------------------------------------------------------------
# attest job
# --------------------------------------------------------------------------


def read_image_records(ctx: Context, control: Mapping[str, Any]) -> dict[str, dict[str, Any]]:
    code = "artifact-invalid:inventory"
    root = ctx.runner_temp() / "ship-images"
    if root.is_symlink() or not root.is_dir():
        common.fail(code)
    expected = {f"ship-image-{component}-{control['run_id']}": component for component in common.COMPONENTS}
    entries = list(os.scandir(root))
    if {entry.name for entry in entries} != set(expected):
        common.fail(code)
    records: dict[str, dict[str, Any]] = {}
    for entry in entries:
        if not stat.S_ISDIR(entry.stat(follow_symlinks=False).st_mode):
            common.fail(code)
        component = expected[entry.name]
        directory = Path(entry.path)
        files = list(os.scandir(directory))
        if {item.name for item in files} != set(IMAGE_FILES):
            common.fail(code)
        for item in files:
            if not stat.S_ISREG(item.stat(follow_symlinks=False).st_mode):
                common.fail(code)
        record = validate_image_record(
            common.load_json(directory / "image.json", "artifact-invalid:image-record"),
            component,
            control["sha"],
        )
        if common.sha256_bytes((directory / "sbom.spdx.json").read_bytes()) != record["sbom_sha256"]:
            common.fail("artifact-invalid:sbom")
        records[component] = record
    digests = [records[component]["digest"] for component in common.COMPONENTS]
    if len(set(digests)) != 3:
        common.fail(code)
    return records


def cmd_candidate(ctx: Context, stage: str) -> int:
    control = common.validate_context(ctx.env)
    records = read_image_records(ctx, control)
    root = ctx.runner_temp() / "ship-images"
    if stage == "read":
        ctx.out.set_outputs(
            {
                "web": records["web"]["digest"],
                "meta_relay": records["meta-relay"]["digest"],
                "gmail_relay": records["gmail-relay"]["digest"],
            }
        )
        ctx.out.text("image records read: 3")
    elif stage == "verify":
        gh = ctx.gh()
        for component in common.COMPONENTS:
            record = records[component]
            suffix = STAGE_SUFFIX[component]
            bundles = []
            for kind in ("PROV", "SBOM"):
                raw = ctx.env.get(f"SHIP_{kind}_BUNDLE_{suffix}")
                if type(raw) is not str or not raw:
                    common.fail("artifact-invalid:bundle")
                path = Path(raw)
                if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
                    common.fail("artifact-invalid:bundle")
                bundles.append(str(path))
            arguments = dict(
                signer_workflow=release_record.SHIP_SIGNER,
                signer_digest=control["workflow_sha"],
                source_digest=control["sha"],
            )
            gh.verify_image(record["image"], record["digest"], predicate_type=release_record.SLSA_PREDICATE,
                            bundle=bundles[0], **arguments)
            sbom_results = gh.verify_image(record["image"], record["digest"],
                                           predicate_type=release_record.SPDX_PREDICATE, bundle=bundles[1], **arguments)
            sbom = common.loads_strict((root / f"ship-image-{component}-{control['run_id']}" / "sbom.spdx.json").read_bytes(),
                                       decimals=True, code="artifact-invalid:sbom")
            if not any(_statement_predicate(item) == sbom for item in sbom_results):
                common.fail("attestation-unverified:sbom")
            gh.verify_image(record["image"], record["digest"], predicate_type=release_record.SLSA_PREDICATE, **arguments)
        ctx.out.text("attestations verified (signer ship.yml, ref main)")
    elif stage == "assemble":
        exceptions = {records[component]["trivy"]["active_exceptions"] for component in common.COMPONENTS}
        if len(exceptions) != 1:
            common.fail("artifact-invalid:trivy")
        candidate = {
            "schema_version": 1,
            "sha": control["sha"],
            "workflow_sha": control["workflow_sha"],
            "built_at": min(records[component]["built_at"] for component in common.COMPONENTS),
            "images": {component: records[component]["digest"] for component in common.COMPONENTS},
            "trivy": {
                "db_updated_at": min(records[component]["trivy"]["db_updated_at"] for component in common.COMPONENTS),
                "active_exceptions": exceptions.pop(),
            },
        }
        validate_candidate(candidate)
        raw = common.canonical_file_bytes(candidate)
        ctx.out.set_outputs(
            {
                "candidate_b64": base64.b64encode(raw).decode("ascii"),
                "candidate_sha256": common.sha256_bytes(raw),
            }
        )
        ctx.out.summary(
            [
                "## Release candidate",
                "",
                f"- Commit: {candidate['sha']}",
                *_digest_lines(candidate["images"]),
                f"- Trivy database updated at: {candidate['trivy']['db_updated_at']}",
                f"- Active Trivy exceptions: {candidate['trivy']['active_exceptions']}",
                "- attestations verified (signer ship.yml, ref main)",
            ]
        )
        ctx.out.text("candidate assembled")
        if ctx.env.get("SHIP_MODE") == BYPASS_MODE:
            ctx.out.summary(["", *BYPASS_BANNER])
    else:
        common.fail("input-invalid:stage")
    return EXIT_OK


def _statement_predicate(item: Any) -> Any:
    try:
        return item["verificationResult"]["statement"]["predicate"]
    except (KeyError, TypeError):
        return None


def validate_candidate(value: Any) -> dict[str, Any]:
    code = "candidate-invalid"
    candidate = common.exact_keys(value, {"schema_version", "sha", "workflow_sha", "built_at", "images", "trivy"}, code)
    if candidate["schema_version"] != 1:
        common.fail(code)
    common.require_sha1(candidate["sha"], code)
    common.require_sha1(candidate["workflow_sha"], code)
    common.require_timestamp(candidate["built_at"], code)
    images = spec_images.require_image_set(candidate["images"], code)
    if len(set(images.values())) != 3:
        common.fail(code)
    trivy = common.exact_keys(candidate["trivy"], {"db_updated_at", "active_exceptions"}, code)
    common.require_timestamp(trivy["db_updated_at"], code)
    common.exact_int(trivy["active_exceptions"], code, 0, trivy_policy.MAX_ENTRIES)
    return candidate


def decode_candidate(env: Mapping[str, str], control: Mapping[str, Any], now: dt.datetime) -> dict[str, Any]:
    code = "candidate-invalid"
    encoded = env.get("CANDIDATE_B64")
    expected = env.get("CANDIDATE_SHA256")
    if type(encoded) is not str or not encoded or re.fullmatch(r"[A-Za-z0-9+/]+={0,2}", encoded) is None:
        common.fail(code)
    try:
        raw = base64.b64decode(encoded, validate=True)
    except ValueError as exc:
        raise common.ReleaseError(code) from exc
    if common.sha256_bytes(raw) != common.require_sha256(expected, code):
        common.fail(code)
    candidate = validate_candidate(common.loads_strict(raw, code=code))
    if raw != common.canonical_file_bytes(candidate):
        common.fail(code)
    if candidate["sha"] != control["sha"] or candidate["workflow_sha"] != control["workflow_sha"]:
        common.fail("candidate-invalid:foreign-sha")
    age = now - common.require_timestamp(candidate["built_at"], code)
    if age < dt.timedelta(0) or age > CANDIDATE_MAX_AGE:
        common.fail("candidate-stale")
    return candidate


# --------------------------------------------------------------------------
# production
# --------------------------------------------------------------------------


def parse_target_secret(raw: Any) -> dict[str, str]:
    code = "target-invalid:production-target"
    if type(raw) is not str or not raw or len(raw) > 4096:
        common.fail(code)
    value = common.exact_keys(
        common.loads_strict(raw, code=code), {"schema_version", "app_id", "postgres_cluster_id"}, code
    )
    if value["schema_version"] != 1:
        common.fail(code)
    app_id = common.require_uuid(value["app_id"], code)
    cluster_id = common.require_uuid(value["postgres_cluster_id"], code)
    if app_id == cluster_id:
        common.fail(code)
    return {"app_id": app_id, "postgres_cluster_id": cluster_id}


def production_worst_case_seconds() -> int:
    """Upper bound of one production run that deploys, fails at the end and
    rolls back: the forward path and the rollback path each run one PUT, a
    reconcile, a settle and a full failing smoke. Each poll loop may overrun
    its wall-clock deadline by one sleep plus one iteration of GETs."""
    get = do_app.GET_TIMEOUT_SECONDS
    reconcile = do_app.RECONCILE_DEADLINE_SECONDS + do_app.POLL_SECONDS + 2 * get
    settle = do_app.SETTLE_DEADLINE_SECONDS + do_app.POLL_SECONDS + 4 * get
    smoke_seconds = (
        smoke.ROUNDS * len(smoke.HEALTH) * smoke.SOCKET_TIMEOUT_SECONDS
        + (smoke.ROUNDS - 1) * smoke.ROUND_DELAY_SECONDS
    )
    forward = do_app.PUT_TIMEOUT_SECONDS + reconcile + settle + smoke_seconds
    rollback = 2 * get + do_app.PUT_TIMEOUT_SECONDS + reconcile + settle + smoke_seconds
    return PREAMBLE_BUDGET_SECONDS + forward + rollback


def _interrupt(signum: int, frame: Any) -> None:
    del signum, frame
    raise KeyboardInterrupt


class _Run:
    """Mutable state of one production run, for exit-code classification."""

    def __init__(self) -> None:
        self.put_attempted = False
        self.clients: list[do_app.DOAppClient] = []


def run_production(ctx: Context) -> int:
    # The protected values leave the process environment before anything
    # else runs, so no subprocess can ever inherit them.
    token = ctx.env.pop("SHIP_DO_TOKEN", None)
    target_raw = ctx.env.pop("SHIP_TARGET_JSON", None)
    out = ctx.out
    run = _Run()
    # GitHub stops a cancelled or timed-out job with SIGINT, then SIGTERM;
    # both end in the classified handler below instead of a bare exit.
    installed = threading.current_thread() is threading.main_thread()
    previous_handler: Any = signal.SIG_DFL
    if installed:
        previous_handler = signal.signal(signal.SIGTERM, _interrupt)
    try:
        return _production(ctx, token if type(token) is str else "", target_raw, run)
    except KeyboardInterrupt:
        if run.put_attempted:
            safe_text(out, "cancelled after the PUT: the job was interrupted")
            safe_text(out, MANUAL_LINE)
            return EXIT_MANUAL
        safe_text(out, "cancelled before the PUT; production unchanged")
        return EXIT_REFUSED
    except schema_change.SchemaChangeBlocked as exc:
        report_schema_block(out, exc)
        return EXIT_MANUAL if run.put_attempted else EXIT_REFUSED
    except common.ReleaseError as exc:
        if run.put_attempted:
            safe_text(out, f"failed after the PUT: {exc.code}")
            safe_text(out, MANUAL_LINE)
            return EXIT_MANUAL
        safe_text(out, f"refused: {exc.code}; production unchanged")
        return EXIT_REFUSED
    except Exception:  # noqa: BLE001 - reported by class-free constant only
        if run.put_attempted:
            safe_text(out, "failed after the PUT: internal-error")
            safe_text(out, MANUAL_LINE)
            return EXIT_MANUAL
        safe_text(out, "refused: internal-error; production unchanged")
        return EXIT_REFUSED
    finally:
        if installed:
            signal.signal(signal.SIGTERM, previous_handler if previous_handler is not None else signal.SIG_DFL)
        for client in run.clients:
            client.scrub()
        token = None
        target_raw = None


def _same_state(left: do_app.Snapshot, right: do_app.Snapshot) -> bool:
    return (left.public, left.spec, left.active_id) == (right.public, right.spec, right.active_id)


def _production(ctx: Context, token: str, target_raw: Any, run: _Run) -> int:
    env = ctx.env
    out = ctx.out
    deps = ctx.deps
    # P0 context, ambient credentials and target identity, before any I/O.
    dispatched_mode, target_release = parse_mode(env)
    # Break-glass uses the existing promote path and manifest kind throughout.
    mode = "promote" if dispatched_mode == BYPASS_MODE else dispatched_mode
    if dispatched_mode not in PRODUCTION_MODES:
        common.fail("input-invalid:production-mode")
    control = common.validate_context(env)
    if any(name in env for name in common.FORBIDDEN_AMBIENT) or any(name.startswith("STAGING_") for name in env):
        common.fail("context-invalid:forbidden-ambient-credential")
    if not token:
        common.fail("target-invalid:token")
    out.add_private(token)
    secret = parse_target_secret(target_raw)
    out.add_mask(secret["app_id"])
    out.add_mask(secret["postgres_cluster_id"])
    target = ctx.ship_target()
    if common.sha256_text(secret["app_id"]) != target["app_id_sha256"]:
        common.fail("app-identity-mismatch")
    sha = control["sha"]
    if dispatched_mode == "promote":
        # These are public same-run outputs, never staging provider credentials.
        # Validate them before GitHub or DigitalOcean access in this command.
        import stage_evidence
        stage_evidence.verify(env, control=control, candidate=decode_candidate(env, control, ctx.now()),
            pins=common.load_json(deps.staging_pins_path, "target-invalid:staging-pins"), production=target)
    # P1 inputs.
    if mode == "rollback":
        if target_release != env.get("PLAN_TARGET_RELEASE"):
            common.fail("latest-changed-since-plan:target")
    elif env.get("PLAN_TARGET_RELEASE", "") != "" or env.get("PLAN_TARGET_MANIFEST_SHA256", "") != "":
        common.fail("input-invalid:plan-target")
    gh = ctx.gh()
    # P1a the owner-only approval rule, before any DigitalOcean I/O: the
    # environment still requires the owner's review, and the owner approved
    # this run's production job.
    release_record.require_approval_gate(gh)
    release_record.require_owner_approval(gh, control["run_id"])
    # P2 old-lane quiescence.
    require_old_lanes_idle(gh, ctx.repo_dir)
    # P3 the verified record chain, bound to what the owner approved.
    bootstrap = ctx.bootstrap()
    chain = release_record.resolve_chain(gh, ctx.repo_dir, bootstrap=bootstrap, work_dir=ctx.work_dir("production-chain"))
    latest = chain.latest
    if latest.release != env.get("PLAN_LATEST_RELEASE") or latest.manifest_sha256 != env.get("PLAN_LATEST_MANIFEST_SHA256"):
        common.fail("latest-changed-since-plan")
    # P4 desired images.
    candidate: dict[str, Any] | None = None
    target_entry: release_record.Entry | None = None
    if mode in {"dry-run", "promote"}:
        candidate = decode_candidate(env, control, ctx.now())
        desired_images = dict(candidate["images"])
        if set(desired_images.values()) & set(bootstrap["images"].values()):
            common.fail("candidate-invalid:bootstrap-digest")
        release_record.verify_images(gh, desired_images, sha, bootstrap)
        release_record.require_no_downgrade(ctx.repo_dir, latest.sha, sha)
        schema_change.guard(ctx.repo_dir, latest.sha, sha)
    else:
        target_entry, _intermediates = release_record.rollback_plan(chain, target_release)
        if target_entry.manifest_sha256 != env.get("PLAN_TARGET_MANIFEST_SHA256"):
            common.fail("latest-changed-since-plan:target")
        release_record.verify_images(gh, target_entry.images, target_entry.sha, bootstrap)
        schema_change.guard(ctx.repo_dir, target_entry.sha, latest.sha)
        desired_images = dict(target_entry.images)
    # P5 two identical double reads, digest sources, topology and VPC.
    client = do_app.DOAppClient(
        secret["app_id"],
        secret["postgres_cluster_id"],
        token,
        expected_app_id_sha256=target["app_id_sha256"],
        allow_put=mode in PUT_MODES,
        opener=deps.opener,
    )
    run.clients.append(client)
    before = do_app.observe_stable(client)
    _mask_observation(out, client, before)
    spec_images.require_digest_sources(before.spec)
    cluster_name = spec_images.require_topology(before.spec, target)
    out.add_mask(cluster_name)
    origin, host = smoke.canonical_origin(before.ingress, target["default_ingress_sha256"])
    out.add_mask(host)
    out.add_mask(origin)
    live_images = spec_images.extract_image_digests(before.spec)
    # P6 drift.
    if mode in {"dry-run", "promote"}:
        if live_images != latest.images:
            common.fail("drift")
        case = mode
    else:
        assert target_entry is not None
        case = release_record.decide_rollback_case(live_images, chain, target_entry)
        desired_images = dict(latest.images if case == "restore" else target_entry.images)
    # P7 backup gate (every mode, dry-run included).
    backup = backup_check.check(client, target=target, now=ctx.now())
    out.text(f"backup gate: newest PostgreSQL backup is {backup['age_hours']} h old (limit 36 h)")
    # P9r rollback reconcile: production already runs the target.
    if case == "reconcile":
        assert target_entry is not None
        health = ctx.health(origin)
        again = do_app.observe_stable(client)
        if not _same_state(again, before):
            common.fail("cas-changed")
        out.text(f"reconcile: production already runs {target_entry.release}; no PUT; health {health['health']}")
        _finish(
            ctx,
            kind="rollback",
            sha=target_entry.sha,
            images=target_entry.images,
            previous=latest.release,
            rolled_back_from=latest.release,
            built_at="",
            reconciled=True,
            state=before,
            summary=[f"- Outcome: reconcile (no PUT); production runs {target_entry.release}"],
        )
        return EXIT_OK
    # P8 the image-only desired spec and the sanitized digest diff.
    desired_spec = spec_images.set_images(before.spec, desired_images)
    changed = spec_images.require_image_only_change(before.spec, desired_spec)
    if spec_images.environment_value_fingerprint(desired_spec) != before.public["environment_values_sha256"]:
        common.fail("topology-differs:environment")
    if spec_images.non_source_fingerprint(desired_spec) != before.public["non_source_projection_sha256"]:
        common.fail("topology-differs:non-source")
    spec_images.require_topology(desired_spec, target)
    diff_lines = [
        f"- {component}: {live_images[component]} -> {desired_images[component]}"
        + ("" if live_images[component] != desired_images[component] else " (unchanged)")
        for component in common.COMPONENTS
    ]
    out.emit(
        {
            "event": "digest-diff",
            "case": case,
            "components": {
                component: {"current": live_images[component], "desired": desired_images[component]}
                for component in common.COMPONENTS
            },
            "changed_spec_leaves": len(changed),
            "note": "environment/topology fingerprints unchanged, VPC bound",
        }
    )
    out.summary(["## Release production", "", f"- Mode: {mode} ({case})", f"- Commit: {sha}", *diff_lines,
                 f"- Changed spec leaves: {len(changed)}; environment/topology fingerprints unchanged, VPC bound"])
    if dispatched_mode == BYPASS_MODE:
        out.summary(["", *BYPASS_BANNER])
    # P9 dry-run: everything except the PUT.
    if mode == "dry-run":
        health = ctx.health(origin)
        again = do_app.observe_stable(client)
        if not _same_state(again, before):
            common.fail("cas-changed")
        if client.put_count() != 0:
            common.fail("internal-error:dry-run-put")
        out.text(f"dry-run complete: no PUT; current production health {health['health']}")
        out.summary(["- Outcome: dry-run complete, no PUT", ""])
        return EXIT_OK
    # P10 CAS immediately before the one PUT.
    cas = do_app.observe_stable(client)
    if not _same_state(cas, before):
        common.fail("cas-changed")
    # P11 exactly one PUT, never retried.
    run.put_attempted = True
    try:
        client.put_app_once(desired_spec)
    except common.AmbiguousMutation:
        out.text("the PUT outcome is ambiguous; reconciling with GETs only")
    except common.ReleaseError as exc:
        if exc.reason != "provider-rejected":
            raise
        try:
            unchanged = _same_state(do_app.observe_stable(client), before)
        except common.ReleaseError:
            unchanged = False
        if unchanged:
            run.put_attempted = False
            out.text("refused: provider-rejected; production unchanged")
            return EXIT_REFUSED
        out.text("provider-rejected, but production no longer equals the pre-PUT state")
        out.text(MANUAL_LINE)
        return EXIT_MANUAL
    # P12 GET-only reconcile until ACTIVE with PRE_DEPLOY SUCCESS on the web digest.
    rollback = _Rollback(ctx, run, token, secret, target, before, desired_spec, live_images, latest, origin)
    try:
        app_response, deployment_response, ambiguous, candidate_id = do_app.reconcile_until_active(
            client,
            desired_spec,
            job_name=common.PRE_DEPLOY_JOB,
            web_digest=desired_images["web"],
            exclude_ids={before.active_id},
            sleeper=deps.sleeper,
            poll_limit=deps.poll_limit,
        )
    except do_app.TerminalDeployment as exc:
        return rollback.execute("deployment-error", exc.deployment_id)
    except do_app.PostDeployGuard as exc:
        return rollback.execute("post-deploy-guard", getattr(exc, "deployment_id", None))
    except do_app.ReconcileTimeout:
        try:
            unchanged = _same_state(do_app.observe_stable(client), before)
        except common.ReleaseError:
            unchanged = False
        out.text("ambiguous-not-observed: the PUT never became visible" if unchanged else "reconcile-timeout")
        out.text(MANUAL_LINE)
        return EXIT_MANUAL
    out.add_mask(candidate_id)
    # P13 post-deploy guards.
    try:
        after = do_app.provider_snapshot(app_response, deployment_response, client.app_id)
        settled = do_app.observe_settled(client, sleeper=deps.sleeper, poll_limit=deps.poll_limit)
        do_app.require_materially_unchanged(after, settled)
        if spec_images.extract_image_digests(settled.spec) != desired_images:
            raise do_app.PostDeployGuard("post-deploy-guard:images")
        for key in ("environment_values_sha256", "non_source_projection_sha256"):
            if settled.public[key] != before.public[key]:
                raise do_app.PostDeployGuard("post-deploy-guard:fingerprint")
        spec_images.require_topology(settled.spec, target)
        do_app.migration_succeeded(settled.deployment, job_name=common.PRE_DEPLOY_JOB, web_digest=desired_images["web"])
        if client.put_count() != 1 or any(method not in {"GET", "PUT"} for method, _ in client.request_log):
            raise do_app.PostDeployGuard("post-deploy-guard:ledger")
    except common.ReleaseError:
        return rollback.execute("post-deploy-guard", candidate_id)
    # P14 blocking smoke in the same job.
    try:
        health = ctx.health(origin)
    except common.ReleaseError:
        return rollback.execute("smoke-failed", candidate_id)
    # P15 success.
    if case == "promote":
        assert candidate is not None
        kind, record_sha, built_at, rolled_back_from = "promote", sha, candidate["built_at"], ""
    elif case == "rollback":
        assert target_entry is not None
        kind, record_sha, built_at, rolled_back_from = "rollback", target_entry.sha, "", latest.release
    else:
        kind, record_sha, built_at, rolled_back_from = "restore", latest.sha, "", ""
    out.text(
        f"{kind} complete: health {health['health']}; one PUT"
        + (" (ambiguous, reconciled)" if ambiguous else "")
    )
    _finish(
        ctx,
        kind=kind,
        sha=record_sha,
        images=desired_images,
        previous=latest.release,
        rolled_back_from=rolled_back_from,
        built_at=built_at,
        reconciled=False,
        state=settled,
        summary=[f"- Outcome: {kind} deployed, health {health['health']}, one PUT"],
    )
    return EXIT_OK


def _mask_observation(out: common.Output, client: do_app.DOAppClient, snapshot: do_app.Snapshot) -> None:
    out.add_mask(snapshot.active_id)
    vpc = snapshot.spec.get("vpc")
    if type(vpc) is dict and type(vpc.get("id")) is str:
        out.add_mask(vpc["id"])
    for identity in sorted(client.observed_ids):
        out.add_mask(identity)


def _finish(
    ctx: Context,
    *,
    kind: str,
    sha: str,
    images: Mapping[str, str],
    previous: str,
    rolled_back_from: str,
    built_at: str,
    reconciled: bool,
    state: do_app.Snapshot,
    summary: Sequence[str],
) -> None:
    deployed_at = common.format_timestamp(ctx.now())
    release = f"prod-{common.compact_timestamp(deployed_at)}-{sha[:8]}"
    outputs = {
        "release": release,
        "kind": kind,
        "sha": sha,
        "previous": previous,
        "rolled_back_from": rolled_back_from,
        "deployed_at": deployed_at,
        "built_at": built_at,
        "reconciled": "true" if reconciled else "false",
        "environment_values_sha256": state.public["environment_values_sha256"],
        "non_source_projection_sha256": state.public["non_source_projection_sha256"],
    }
    for component in common.COMPONENTS:
        outputs[OUTPUT_DIGEST_KEYS[component]] = images[component]
    ctx.out.set_outputs(outputs)
    ctx.out.summary([*summary, f"- Record to write: {release}", ""])


class _Rollback:
    """AUTO-ROLLBACK: a separate client with its own single-PUT budget,
    re-PUTting the pre-PUT spec only if the live spec still equals what this
    run PUT."""

    def __init__(
        self,
        ctx: Context,
        run: _Run,
        token: str,
        secret: Mapping[str, str],
        target: Mapping[str, Any],
        before: do_app.Snapshot,
        desired_spec: Mapping[str, Any],
        live_images: Mapping[str, str],
        latest: release_record.Entry,
        origin: str,
    ) -> None:
        self.ctx = ctx
        self.run = run
        self._token = token
        self.secret = secret
        self.target = target
        self.before = before
        self.desired_spec = desired_spec
        self.live_images = dict(live_images)
        self.latest = latest
        self.origin = origin

    def _manual(self, reason: str) -> int:
        safe_text(self.ctx.out, f"automatic rollback stopped: {reason}")
        safe_text(self.ctx.out, MANUAL_LINE)
        return EXIT_MANUAL

    def execute(self, reason: str, failed_id: str | None) -> int:
        ctx = self.ctx
        out = ctx.out
        deps = ctx.deps
        out.add_mask(failed_id)
        safe_text(out, f"{reason}: starting the automatic rollback to the pre-PUT spec")
        rollback_client = do_app.DOAppClient(
            self.secret["app_id"],
            self.secret["postgres_cluster_id"],
            self._token,
            expected_app_id_sha256=self.target["app_id_sha256"],
            allow_put=True,
            opener=deps.opener,
        )
        self.run.clients.append(rollback_client)
        if not do_app.observe_for_rollback(rollback_client, self.desired_spec):
            return self._manual("rollback-precondition-failed (the live spec no longer equals what this run PUT)")
        try:
            rollback_client.put_app_once(self.before.spec)
        except common.AmbiguousMutation:
            safe_text(out, "the rollback PUT outcome is ambiguous; reconciling with GETs only")
        except common.ReleaseError:
            return self._manual("rollback-failed:provider-rejected")
        excluded = {self.before.active_id}
        if failed_id:
            excluded.add(failed_id)
        try:
            do_app.reconcile_until_active(
                rollback_client,
                self.before.spec,
                job_name=common.PRE_DEPLOY_JOB,
                web_digest=self.live_images["web"],
                exclude_ids=excluded,
                sleeper=deps.sleeper,
                poll_limit=deps.poll_limit,
            )
            settled = do_app.observe_settled(rollback_client, sleeper=deps.sleeper, poll_limit=deps.poll_limit)
            if spec_images.extract_image_digests(settled.spec) != self.live_images:
                common.fail("rollback-failed:images")
            if rollback_client.put_count() != 1:
                common.fail("rollback-failed:ledger")
        except common.ReleaseError as exc:
            return self._manual(exc.code if exc.reason == "rollback-failed" else "rollback-failed:reconcile")
        try:
            health = ctx.health(self.origin)
        except common.ReleaseError:
            return self._manual("rollback-failed:health")
        restored = (
            self.latest.release if self.live_images == self.latest.images else "the pre-run state"
        )
        safe_text(out, f"{reason}; rolled back to {restored}; health {health['health']}; no record written")
        try:
            out.summary([f"- Outcome: {reason}; automatically rolled back to {restored}", ""])
        except (common.ReleaseError, OSError):
            pass
        return EXIT_ROLLED_BACK


# --------------------------------------------------------------------------
# record job
# --------------------------------------------------------------------------


RECORD_FIELDS = (
    "RELEASE", "KIND", "SHA", "WEB_DIGEST", "META_RELAY_DIGEST", "GMAIL_RELAY_DIGEST",
    "PREVIOUS", "ROLLED_BACK_FROM", "DEPLOYED_AT", "BUILT_AT", "RECONCILED",
    "ENVIRONMENT_VALUES_SHA256", "NON_SOURCE_PROJECTION_SHA256",
)


def _record_fields(env: Mapping[str, str], control: Mapping[str, Any]) -> dict[str, Any]:
    code = "record-invalid:inputs"
    raw = {name: env.get(f"REC_{name}", "") for name in RECORD_FIELDS}
    if any(type(value) is not str for value in raw.values()):
        common.fail(code)
    for value in raw.values():
        if value and common.OUTPUT_VALUE_RE.fullmatch(value) is None:
            common.fail(code)
    if raw["RECONCILED"] not in {"true", "false"}:
        common.fail(code)
    fields = {
        "kind": raw["KIND"],
        "release": raw["RELEASE"],
        "sha": raw["SHA"],
        "images": {
            "web": raw["WEB_DIGEST"],
            "meta-relay": raw["META_RELAY_DIGEST"],
            "gmail-relay": raw["GMAIL_RELAY_DIGEST"],
        },
        "previous": raw["PREVIOUS"],
        "rolled_back_from": raw["ROLLED_BACK_FROM"] or None,
        "reconciled": raw["RECONCILED"] == "true",
        "built_at": raw["BUILT_AT"] or None,
        "deployed_at": raw["DEPLOYED_AT"],
        "workflow_sha": control["workflow_sha"],
        "environment_values_sha256": raw["ENVIRONMENT_VALUES_SHA256"],
        "non_source_projection_sha256": raw["NON_SOURCE_PROJECTION_SHA256"],
    }
    try:
        release_record.build_manifest(fields)
    except common.ReleaseError as exc:
        raise common.ReleaseError(code) from exc
    if fields["kind"] == "promote" and fields["sha"] != control["sha"]:
        common.fail(code)
    return fields


def _record_paths(ctx: Context) -> tuple[Path, Path]:
    directory = ctx.runner_temp() / "ship-record"
    return directory, directory / release_record.MANIFEST_ASSET


def _find_draft(gh: release_record.Gh, tag: str) -> bool:
    drafts = [
        release for release in gh.releases()
        if type(release) is dict and release.get("tag_name") == tag and release.get("draft") is True
    ]
    if len(drafts) > 1:
        common.fail("record-invalid:duplicate-draft")
    return bool(drafts)


def _record_state(ctx: Context, gh: release_record.Gh, raw: bytes, manifest: Mapping[str, Any]) -> str:
    chain = release_record.resolve_chain(
        gh, None, bootstrap=ctx.bootstrap(), work_dir=ctx.work_dir("record-chain")
    )
    index = chain.find(manifest["release"])
    if index is not None:
        if index != len(chain.entries) - 1:
            common.fail("latest-changed-since-production")
        if common.canonical_file_bytes(chain.latest.document) != raw:
            common.fail("record-invalid:published-differs")
        return "published"
    if chain.latest.release != manifest["previous"]:
        common.fail("latest-changed-since-production")
    # The new record must extend the verified chain exactly (a published
    # record that breaks the chain would wedge every later run).
    release_record.require_link(list(chain.entries), release_record.manifest_entry(manifest, raw), None)
    return "draft" if _find_draft(gh, manifest["release"]) else "new"


def _load_local_manifest(ctx: Context) -> tuple[bytes, dict[str, Any], Path]:
    _directory, path = _record_paths(ctx)
    if path.is_symlink() or not path.is_file():
        common.fail("record-invalid:local")
    raw = path.read_bytes()
    manifest = release_record.validate_manifest(common.loads_strict(raw, code="record-invalid:local"))
    if raw != common.canonical_file_bytes(manifest):
        common.fail("record-invalid:local")
    return raw, manifest, path


def render_notes(manifest: Mapping[str, Any], *, mode: str = "") -> list[str]:
    lines = [
        f"Release record {manifest['release']} ({manifest['kind']}).",
        "",
        "Written by the Release workflow (.github/workflows/ship.yml). The attested asset "
        "release-manifest.json is the record; these notes are informational only.",
        "",
        f"- Commit: {manifest['sha']}",
        *_digest_lines(manifest["images"]),
        f"- Previous record: {manifest['previous']}",
    ]
    if manifest["rolled_back_from"]:
        lines.append(f"- Rolled back from: {manifest['rolled_back_from']}")
    if manifest["reconciled"]:
        lines.append("- Reconciled: production already ran these digests; no PUT")
    if mode == BYPASS_MODE:
        lines += ["", *BYPASS_BANNER]
    return lines


def cmd_record(ctx: Context, stage: str) -> int:
    control = common.validate_context(ctx.env, allow_rerun=True)
    out = ctx.out
    gh = ctx.gh()
    if stage == "manifest":
        fields = _record_fields(ctx.env, control)
        raw = release_record.build_manifest(fields)
        manifest = release_record.validate_manifest(common.loads_strict(raw, code="record-invalid"))
        state = _record_state(ctx, gh, raw, manifest)
        directory, path = _record_paths(ctx)
        directory.mkdir(mode=0o700, exist_ok=True)
        if path.exists() and path.read_bytes() != raw:
            common.fail("record-invalid:local")
        path.write_bytes(raw)
        out.set_outputs({"tag": manifest["release"], "path": str(path).replace("\\", "/"), "state": state})
        out.text(f"record {manifest['release']}: {state}")
    elif stage == "publish":
        raw, manifest, path = _load_local_manifest(ctx)
        state = _record_state(ctx, gh, raw, manifest)
        tag = manifest["release"]
        if state == "published":
            out.text(f"record {tag} is already published")
            return EXIT_OK
        notes = path.parent / "notes.md"
        lines = [common.require_public_text(line) for line in render_notes(manifest, mode=ctx.env.get("SHIP_MODE", ""))]
        notes.write_bytes(("\n".join(lines) + "\n").encode("ascii"))
        if state == "new":
            gh.run(
                ["release", "create", tag, "--repo", common.REPOSITORY, "--draft", "--target",
                 manifest["sha"], "--title", tag, "--notes-file", str(notes)],
                code="gh-failed:release-create",
            )
            gh.run(["release", "upload", tag, str(path), "--repo", common.REPOSITORY], code="gh-failed:release-upload")
        else:
            gh.run(
                ["release", "upload", tag, str(path), "--repo", common.REPOSITORY, "--clobber"],
                code="gh-failed:release-upload",
            )
        gh.run(["release", "edit", tag, "--repo", common.REPOSITORY, "--draft=false"], code="gh-failed:release-publish")
        out.text(f"record {tag} published")
    elif stage == "verify":
        raw, manifest, path = _load_local_manifest(ctx)
        tag = manifest["release"]
        last_error: common.ReleaseError | None = None
        for attempt in range(6):
            try:
                downloaded = gh.download_manifest(tag, ctx.work_dir("record-verify") / f"attempt-{attempt}")
                if downloaded != raw:
                    common.fail("record-invalid:published-differs")
                workflow_sha = manifest["signer"]["workflow_sha"]
                gh.verify_file(path, signer_digest=workflow_sha, source_digest=workflow_sha)
                chain = release_record.resolve_chain(
                    gh, None, bootstrap=ctx.bootstrap(), work_dir=ctx.work_dir(f"record-verify-chain-{attempt}")
                )
                if chain.latest.release != tag or common.canonical_file_bytes(chain.latest.document) != raw:
                    common.fail("record-invalid:not-latest")
                last_error = None
                break
            except common.ReleaseError as exc:
                last_error = exc
                if attempt < 5:
                    ctx.deps.sleeper(10)
        if last_error is not None:
            raise last_error
        out.text(f"record {tag} verified: attested, tag bound to {manifest['sha']}, latest in the chain")
        out.summary(["## Release record", "", *render_notes(manifest, mode=ctx.env.get("SHIP_MODE", "")), ""])
    else:
        common.fail("input-invalid:stage")
    return EXIT_OK


# --------------------------------------------------------------------------
# entry point
# --------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="ship.py",
        description="Release workflow controls (.github/workflows/ship.yml).",
    )
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("plan", help="verify CI, records and guards before approval")
    image = commands.add_parser("image", help="images job stages")
    image.add_argument("--stage", required=True, choices=("pins", "contract", "trivy-db", "record"))
    image.add_argument("--component", required=True, choices=common.COMPONENTS)
    commands.add_parser("trivy-policy", help="validate the reviewed expiring exceptions")
    candidate = commands.add_parser("candidate", help="attest job stages")
    candidate.add_argument("--stage", required=True, choices=("read", "verify", "assemble"))
    commands.add_parser("production", help="verify, guard, deploy, smoke and roll back")
    record = commands.add_parser("record", help="record job stages")
    record.add_argument("--stage", required=True, choices=("manifest", "publish", "verify"))
    return parser


def main(
    argv: Sequence[str] | None = None,
    env: MutableMapping[str, str] | None = None,
    deps: Deps | None = None,
) -> int:
    environment = os.environ if env is None else env
    dependencies = deps or Deps()
    arguments = build_parser().parse_args(argv)
    out = common.Output(
        stdout=dependencies.stdout or sys.stdout,
        summary_path=environment.get("GITHUB_STEP_SUMMARY"),
        output_path=environment.get("GITHUB_OUTPUT"),
    )
    ctx = Context(environment, dependencies, out)
    if arguments.command == "production":
        return run_production(ctx)
    try:
        if arguments.command == "plan":
            return cmd_plan(ctx)
        if arguments.command == "image":
            return cmd_image(ctx, arguments.stage, arguments.component)
        if arguments.command == "trivy-policy":
            return cmd_trivy_policy(ctx)
        if arguments.command == "candidate":
            return cmd_candidate(ctx, arguments.stage)
        if arguments.command == "record":
            return cmd_record(ctx, arguments.stage)
        common.fail("input-invalid:command")
    except schema_change.SchemaChangeBlocked as exc:
        report_schema_block(out, exc)
        return EXIT_REFUSED
    except common.ReleaseError as exc:
        safe_text(out, f"refused: {exc.code}")
        return EXIT_REFUSED
    except Exception:  # noqa: BLE001 - never print a traceback or a value
        safe_text(out, "refused: internal-error")
        return EXIT_REFUSED


if __name__ == "__main__":
    sys.exit(main())
