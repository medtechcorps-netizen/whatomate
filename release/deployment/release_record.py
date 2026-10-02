#!/usr/bin/env python3
"""Release records for ship.yml: release #0, manifests, the verified record
chain, image attestations, no-downgrade and rollback planning.

A record is a GitHub Release ``prod-<YYYYMMDDTHHMMSSZ>-<sha8>`` whose single
asset ``release-manifest.json`` is attested by ship.yml. Release #0
(``prod-0000``) is the committed, hash-pinned ship-bootstrap-record.json; a
GitHub Release or tag with that name is never consulted. The whole chain is
verified before any record is trusted, and any anomaly fails closed.
"""

from __future__ import annotations

import datetime as dt
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, NamedTuple, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import spec_images


MANIFEST_ASSET = "release-manifest.json"
SHIP_SIGNER = common.SHIP_WORKFLOW_PATH
SLSA_PREDICATE = "https://slsa.dev/provenance/v1"
SPDX_PREDICATE = "https://spdx.dev/Document/v2.3"
# build-attest-exact-release-images.yml:41, the old lane's source binding.
LEGACY_IMAGE_PREDICATE = "https://rereply.app/attestations/exact-release-image/v1"
BOOTSTRAP_PATH = Path(__file__).resolve().with_name("ship-bootstrap-record.json")
# sha256 of the committed ship-bootstrap-record.json bytes.
BOOTSTRAP_SHA256 = "a303f4cc2be58bba0c9653a828bbb6c220b871b73a913fdf8b2d79c519503951"
KINDS = ("promote", "rollback", "restore")
MAX_RECORDS = 500
MAX_MANIFEST_BYTES = 64 * 1024
MAX_GH_OUTPUT_BYTES = 64 * 1024 * 1024
GH_TIMEOUT_SECONDS = 180
MANIFEST_KEYS = frozenset(
    {
        "schema_version", "kind", "release", "sha", "images", "db_change",
        "schema_guard", "previous", "rolled_back_from", "reconciled", "built_at",
        "deployed_at", "signer", "fingerprints",
    }
)
BOOTSTRAP_KEYS = frozenset(
    {
        "schema_version", "kind", "release", "sha", "images", "db_change",
        "previous", "deployed_at", "legacy_signer", "evidence",
    }
)
ACTIVE_RUN_STATUSES = ("in_progress", "pending", "queued", "requested", "waiting")


class Entry(NamedTuple):
    release: str
    kind: str
    sha: str
    images: dict[str, str]
    manifest_sha256: str
    deployed_at: str
    previous: str | None
    rolled_back_from: str | None
    db_change: bool
    document: dict[str, Any]


class Chain(NamedTuple):
    entries: tuple[Entry, ...]

    @property
    def bootstrap(self) -> Entry:
        return self.entries[0]

    @property
    def latest(self) -> Entry:
        return self.entries[-1]

    def find(self, release: str) -> int | None:
        for index, entry in enumerate(self.entries):
            if entry.release == release:
                return index
        return None


# --------------------------------------------------------------------------
# GitHub CLI and git
# --------------------------------------------------------------------------


class Gh:
    """The pinned gh binary with a scrubbed environment and no stderr echo."""

    def __init__(
        self,
        gh_bin: str,
        token: str,
        repo: str,
        *,
        expected_version: str,
        runner: Callable[..., Any] = subprocess.run,
        base_env: Mapping[str, str] | None = None,
        config_dir: str | None = None,
    ) -> None:
        if type(gh_bin) is not str or not gh_bin or any(ch in gh_bin for ch in "\r\n\x00"):
            common.fail("gh-failed:binary")
        if type(token) is not str or not token or any(ch in token for ch in "\r\n\x00 "):
            common.fail("gh-failed:token")
        if repo != common.REPOSITORY:
            common.fail("gh-failed:repository")
        if type(expected_version) is not str or re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", expected_version) is None:
            common.fail("gh-failed:version")
        self.gh_bin = gh_bin
        self.repo = repo
        self._runner = runner
        self._config_dir = config_dir or tempfile.mkdtemp(prefix="ship-gh-")
        self._env = common.scrubbed_env(
            {
                "GH_TOKEN": token,
                "GH_CONFIG_DIR": self._config_dir,
                "GH_PROMPT_DISABLED": "1",
                "GH_NO_UPDATE_NOTIFIER": "1",
                "NO_COLOR": "1",
                "GIT_TERMINAL_PROMPT": "0",
            },
            base=base_env,
        )
        first = self._run(["--version"], code="gh-failed:version").decode("ascii", "replace").splitlines()
        words = first[0].split() if first else []
        if len(words) < 3 or words[0] != "gh" or words[1] != "version" or words[2] != expected_version:
            common.fail("gh-failed:version")

    def _run(self, args: Sequence[str], *, code: str) -> bytes:
        argv = [self.gh_bin, *args]
        try:
            result = self._runner(
                argv,
                env=dict(self._env),
                stdin=subprocess.DEVNULL,
                capture_output=True,
                timeout=GH_TIMEOUT_SECONDS,
                check=False,
            )
        except (OSError, subprocess.SubprocessError, ValueError) as exc:
            raise common.ReleaseError(code) from exc
        if result.returncode != 0:
            common.fail(code)
        stdout = result.stdout
        if type(stdout) is not bytes or len(stdout) > MAX_GH_OUTPUT_BYTES:
            common.fail(code)
        return stdout

    def _json(self, args: Sequence[str], *, code: str) -> Any:
        return common.loads_strict(self._run(args, code=code), decimals=True, code=code)

    def api_list(self, path: str, *, code: str) -> list[Any]:
        pages = self._json(["api", "--paginate", "--slurp", path], code=code)
        if type(pages) is not list:
            common.fail(code)
        output: list[Any] = []
        for page in pages:
            if type(page) is not list:
                common.fail(code)
            output.extend(page)
        return output

    def api_pages(self, path: str, key: str, *, code: str) -> list[Any]:
        pages = self._json(["api", "--paginate", "--slurp", path], code=code)
        if type(pages) is not list:
            common.fail(code)
        output: list[Any] = []
        for page in pages:
            if type(page) is not dict or type(page.get(key)) is not list:
                common.fail(code)
            output.extend(page[key])
        return output

    def releases(self) -> list[Any]:
        return self.api_list(f"/repos/{self.repo}/releases?per_page=100", code="gh-failed:releases")

    def prod_tags(self) -> list[Any]:
        return self.api_list(f"/repos/{self.repo}/git/matching-refs/tags/prod-", code="gh-failed:tags")

    def workflow_runs(self, workflow_file: str, sha: str) -> list[Any]:
        if re.fullmatch(r"[a-z0-9-]+\.yml", workflow_file) is None:
            common.fail("internal-error:workflow-file")
        common.require_sha1(sha, "internal-error:sha")
        path = (
            f"/repos/{self.repo}/actions/workflows/{workflow_file}/runs"
            f"?head_sha={sha}&event=push&branch=main&per_page=100"
        )
        return self.api_pages(path, "workflow_runs", code="gh-failed:workflow-runs")

    def active_runs(self, statuses: Iterable[str] = ACTIVE_RUN_STATUSES) -> list[Any]:
        runs: list[Any] = []
        for status in sorted(set(statuses)):
            if status not in ACTIVE_RUN_STATUSES:
                common.fail("internal-error:run-status")
            runs.extend(
                self.api_pages(
                    f"/repos/{self.repo}/actions/runs?status={status}&per_page=100",
                    "workflow_runs",
                    code="gh-failed:active-runs",
                )
            )
        return runs

    def download_manifest(self, tag: str, directory: Path) -> bytes:
        common.exact_string(tag, "internal-error:tag", common.RECORD_TAG_RE)
        if directory.exists():
            shutil.rmtree(directory)
        directory.mkdir(parents=True)
        self._run(
            [
                "release", "download", tag, "--repo", self.repo,
                "--pattern", MANIFEST_ASSET, "--dir", str(directory),
            ],
            code="gh-failed:download",
        )
        entries = list(directory.iterdir())
        if [entry.name for entry in entries] != [MANIFEST_ASSET]:
            common.fail("record-chain-invalid:asset-inventory")
        path = entries[0]
        if path.is_symlink() or not path.is_file():
            common.fail("record-chain-invalid:asset-inventory")
        raw = path.read_bytes()
        if not raw or len(raw) > MAX_MANIFEST_BYTES:
            common.fail("record-chain-invalid:asset-size")
        return raw

    def _verify(self, subject: str, flags: Sequence[str], *, code: str) -> list[dict[str, Any]]:
        value = self._json(["attestation", "verify", subject, *flags, "--format", "json"], code=code)
        if type(value) is not list or not value or any(type(item) is not dict for item in value):
            common.fail(code)
        return value

    def signer_flags(
        self,
        signer_workflow: str,
        signer_digest: str,
        source_digest: str,
        predicate_type: str,
    ) -> list[str]:
        common.require_sha1(signer_digest, "internal-error:signer-digest")
        common.require_sha1(source_digest, "internal-error:source-digest")
        if re.fullmatch(r"\.github/workflows/[a-z0-9-]+\.yml", signer_workflow) is None:
            common.fail("internal-error:signer-workflow")
        return [
            "--repo", self.repo,
            "--signer-workflow", f"{self.repo}/{signer_workflow}",
            "--signer-digest", signer_digest,
            "--source-digest", source_digest,
            "--source-ref", "refs/heads/main",
            "--deny-self-hosted-runners",
            "--predicate-type", predicate_type,
        ]

    def verify_file(self, path: Path, *, signer_digest: str, source_digest: str) -> list[dict[str, Any]]:
        return self._verify(
            str(path),
            self.signer_flags(SHIP_SIGNER, signer_digest, source_digest, SLSA_PREDICATE),
            code="attestation-unverified:record",
        )

    def verify_image(
        self,
        image: str,
        digest: str,
        *,
        signer_workflow: str,
        signer_digest: str,
        source_digest: str,
        predicate_type: str,
        bundle: str | None = None,
    ) -> list[dict[str, Any]]:
        if image not in common.IMAGE_REPOSITORY.values():
            common.fail("internal-error:image")
        common.require_digest(digest, "internal-error:digest")
        flags = self.signer_flags(signer_workflow, signer_digest, source_digest, predicate_type)
        if bundle is not None:
            flags += ["--bundle", bundle]
        return self._verify(f"oci://{image}@{digest}", flags, code="attestation-unverified:image")

    def run(self, args: Sequence[str], *, code: str) -> bytes:
        """A reviewed write (the record job only)."""
        return self._run(args, code=code)


def git(repo_dir: Path, args: Sequence[str], *, code: str, runner: Callable[..., Any] = subprocess.run) -> Any:
    env = common.scrubbed_env({"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"})
    try:
        return runner(
            ["git", "-C", str(repo_dir), *args],
            env=env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            timeout=600,
            check=False,
        )
    except (OSError, subprocess.SubprocessError, ValueError) as exc:
        raise common.ReleaseError(code) from exc


def git_output(repo_dir: Path, args: Sequence[str], *, code: str) -> bytes:
    result = git(repo_dir, args, code=code)
    if result.returncode != 0:
        common.fail(code)
    return result.stdout


def require_commit(repo_dir: Path, sha: str, code: str) -> None:
    common.require_sha1(sha, code)
    out = git_output(repo_dir, ["cat-file", "-t", sha], code=code).strip()
    if out != b"commit":
        common.fail(code)


def is_ancestor(repo_dir: Path, ancestor: str, descendant: str) -> bool:
    require_commit(repo_dir, ancestor, "downgrade-refused:unknown-commit")
    require_commit(repo_dir, descendant, "downgrade-refused:unknown-commit")
    result = git(repo_dir, ["merge-base", "--is-ancestor", ancestor, descendant], code="git-failed:merge-base")
    if result.returncode == 0:
        return True
    if result.returncode == 1:
        return False
    common.fail("git-failed:merge-base")


def require_no_downgrade(repo_dir: Path, latest_sha: str, candidate_sha: str) -> None:
    """The candidate must descend from (or equal) the latest record's SHA."""
    if not is_ancestor(repo_dir, latest_sha, candidate_sha):
        common.fail("downgrade-refused")


# --------------------------------------------------------------------------
# Release #0 and manifests
# --------------------------------------------------------------------------


def _images(value: Any, code: str) -> dict[str, str]:
    return spec_images.require_image_set(value, code)


def validate_bootstrap(value: Any) -> dict[str, Any]:
    code = "record-chain-invalid:bootstrap"
    record = common.exact_keys(value, BOOTSTRAP_KEYS, code)
    if (
        record["schema_version"] != 1
        or record["kind"] != "bootstrap"
        or record["release"] != common.BOOTSTRAP_TAG
        or record["db_change"] is not False
        or record["previous"] is not None
    ):
        common.fail(code)
    common.require_sha1(record["sha"], code)
    _images(record["images"], code)
    if len(set(record["images"].values())) != 3:
        common.fail(code)
    common.require_timestamp(record["deployed_at"], code)
    legacy = common.exact_keys(
        record["legacy_signer"],
        {"workflow", "workflow_sha", "source_ref", "source_predicate", "source_commit", "phase"},
        code,
    )
    if (
        legacy["workflow"] != ".github/workflows/build-attest-exact-release-images.yml"
        or legacy["source_ref"] != "refs/heads/main"
        or legacy["source_predicate"] != LEGACY_IMAGE_PREDICATE
        or legacy["source_commit"] != record["sha"]
        or legacy["phase"] != "ui"
    ):
        common.fail(code)
    common.require_sha1(legacy["workflow_sha"], code)
    evidence = common.exact_keys(
        record["evidence"],
        {"phase_state_sha256", "phase_state_signer", "phase_state_signer_sha", "phase_state_completed_at"},
        code,
    )
    common.require_sha256(evidence["phase_state_sha256"], code)
    if evidence["phase_state_signer"] != ".github/workflows/verify-production-crm-canary.yml":
        common.fail(code)
    common.require_sha1(evidence["phase_state_signer_sha"], code)
    common.require_timestamp(evidence["phase_state_completed_at"], code)
    return record


def load_bootstrap(path: Path = BOOTSTRAP_PATH, expected_sha256: str = BOOTSTRAP_SHA256) -> dict[str, Any]:
    code = "record-chain-invalid:bootstrap"
    if path.is_symlink() or not path.is_file():
        common.fail(code)
    raw = path.read_bytes()
    if common.sha256_bytes(raw) != expected_sha256:
        common.fail(code)
    value = common.loads_strict(raw, code=code)
    if raw != common.canonical_file_bytes(value):
        common.fail(code)
    return validate_bootstrap(value)


def bootstrap_entry(record: Mapping[str, Any], manifest_sha256: str | None = None) -> Entry:
    """Release #0. Its manifest hash is the hash of its canonical bytes, which
    load_bootstrap has already proven equal to the pinned file hash."""
    return Entry(
        release=common.BOOTSTRAP_TAG,
        kind="bootstrap",
        sha=record["sha"],
        images=dict(record["images"]),
        manifest_sha256=manifest_sha256 or common.sha256_bytes(common.canonical_file_bytes(dict(record))),
        deployed_at=record["deployed_at"],
        previous=None,
        rolled_back_from=None,
        db_change=False,
        document=dict(record),
    )


def _tag_or_bootstrap(value: Any, code: str) -> str:
    if value == common.BOOTSTRAP_TAG:
        return value
    return common.exact_string(value, code, common.RECORD_TAG_RE)


def validate_manifest(value: Any) -> dict[str, Any]:
    code = "record-invalid"
    manifest = common.exact_keys(value, MANIFEST_KEYS, code)
    if manifest["schema_version"] != 1 or manifest["kind"] not in KINDS:
        common.fail(code)
    release = common.exact_string(manifest["release"], code, common.RECORD_TAG_RE)
    sha = common.require_sha1(manifest["sha"], code)
    _images(manifest["images"], code)
    if manifest["db_change"] is not False or manifest["schema_guard"] != "clean":
        common.fail(code)
    previous = _tag_or_bootstrap(manifest["previous"], code)
    deployed_at = manifest["deployed_at"]
    deployed = common.require_timestamp(deployed_at, code)
    match = common.RECORD_TAG_RE.fullmatch(release)
    if match is None or match.group(1) != common.compact_timestamp(deployed_at) or match.group(2) != sha[:8]:
        common.fail(code)
    common.exact_bool(manifest["reconciled"], code)
    signer = common.exact_keys(manifest["signer"], {"workflow", "workflow_sha"}, code)
    if signer["workflow"] != SHIP_SIGNER:
        common.fail(code)
    common.require_sha1(signer["workflow_sha"], code)
    fingerprints = common.exact_keys(
        manifest["fingerprints"], {"environment_values_sha256", "non_source_projection_sha256"}, code
    )
    for item in fingerprints.values():
        common.require_sha256(item, code)
    kind = manifest["kind"]
    rolled_back_from = manifest["rolled_back_from"]
    built_at = manifest["built_at"]
    if kind == "promote":
        if rolled_back_from is not None or manifest["reconciled"] is not False:
            common.fail(code)
        built = common.require_timestamp(built_at, code)
        if built > deployed:
            common.fail(code)
    elif kind == "rollback":
        if built_at is not None:
            common.fail(code)
        if common.exact_string(rolled_back_from, code, common.RECORD_TAG_RE) != previous:
            common.fail(code)
    else:
        if built_at is not None or rolled_back_from is not None or manifest["reconciled"] is not False:
            common.fail(code)
    raw = common.canonical_file_bytes(manifest)
    if len(raw) > MAX_MANIFEST_BYTES:
        common.fail(code)
    return manifest


def build_manifest(fields: Mapping[str, Any]) -> bytes:
    manifest = {
        "schema_version": 1,
        "kind": fields["kind"],
        "release": fields["release"],
        "sha": fields["sha"],
        "images": {component: fields["images"][component] for component in common.COMPONENTS},
        "db_change": False,
        "schema_guard": "clean",
        "previous": fields["previous"],
        "rolled_back_from": fields["rolled_back_from"],
        "reconciled": fields["reconciled"],
        "built_at": fields["built_at"],
        "deployed_at": fields["deployed_at"],
        "signer": {"workflow": SHIP_SIGNER, "workflow_sha": fields["workflow_sha"]},
        "fingerprints": {
            "environment_values_sha256": fields["environment_values_sha256"],
            "non_source_projection_sha256": fields["non_source_projection_sha256"],
        },
    }
    validate_manifest(manifest)
    return common.canonical_file_bytes(manifest)


def manifest_entry(manifest: Mapping[str, Any], raw: bytes) -> Entry:
    return Entry(
        release=manifest["release"],
        kind=manifest["kind"],
        sha=manifest["sha"],
        images=dict(manifest["images"]),
        manifest_sha256=common.sha256_bytes(raw),
        deployed_at=manifest["deployed_at"],
        previous=manifest["previous"],
        rolled_back_from=manifest["rolled_back_from"],
        db_change=manifest["db_change"],
        document=dict(manifest),
    )


def require_link(entries: Sequence[Entry], entry: Entry, repo_dir: Path | None) -> None:
    """Previous links, strictly increasing time and the per-kind rules."""
    code = "record-chain-invalid:link"
    previous = entries[-1]
    if entry.previous != previous.release:
        common.fail(code)
    if common.require_timestamp(entry.deployed_at, code) <= common.require_timestamp(previous.deployed_at, code):
        common.fail(code)
    if entry.db_change is not False:
        common.fail(code)
    if entry.kind == "promote":
        if repo_dir is not None and not is_ancestor(repo_dir, previous.sha, entry.sha):
            common.fail(code)
    elif entry.kind == "rollback":
        if entry.rolled_back_from != previous.release:
            common.fail(code)
        if not any((older.sha, older.images) == (entry.sha, entry.images) for older in entries[:-1]):
            common.fail(code)
    elif entry.kind == "restore":
        if (entry.sha, entry.images) != (previous.sha, previous.images) or entry.rolled_back_from is not None:
            common.fail(code)
    else:
        common.fail(code)


def resolve_chain(
    gh: Gh,
    repo_dir: Path | None,
    *,
    bootstrap: Mapping[str, Any],
    work_dir: Path,
) -> Chain:
    """Section 7.2: every record verified, linked and tag-bound, or nothing."""
    releases = gh.releases()
    refs = gh.prod_tags()
    published: dict[str, dict[str, Any]] = {}
    for release in releases:
        if type(release) is not dict or type(release.get("tag_name")) is not str:
            common.fail("record-chain-invalid:release")
        tag = release["tag_name"]
        if not tag.startswith("prod-") or tag == common.BOOTSTRAP_TAG:
            continue
        if release.get("draft") is True:
            # A draft is not a record. Read-only tokens never see drafts, so
            # every job resolves the same published chain.
            continue
        if common.RECORD_TAG_RE.fullmatch(tag) is None:
            common.fail("record-chain-invalid:tag-format")
        if release.get("draft") is not False or release.get("prerelease") is not False:
            common.fail("record-chain-invalid:release-state")
        if tag in published:
            common.fail("record-chain-invalid:duplicate")
        assets = release.get("assets")
        if (
            type(assets) is not list
            or len(assets) != 1
            or type(assets[0]) is not dict
            or assets[0].get("name") != MANIFEST_ASSET
            or type(assets[0].get("size")) is not int
            or not 0 < assets[0]["size"] <= MAX_MANIFEST_BYTES
        ):
            common.fail("record-chain-invalid:assets")
        published[tag] = release
    tags: dict[str, str] = {}
    for ref in refs:
        if type(ref) is not dict or type(ref.get("ref")) is not str or type(ref.get("object")) is not dict:
            common.fail("record-chain-invalid:tag")
        name = ref["ref"]
        if not name.startswith("refs/tags/prod-"):
            common.fail("record-chain-invalid:tag")
        tag = name[len("refs/tags/"):]
        if tag == common.BOOTSTRAP_TAG:
            continue
        if common.RECORD_TAG_RE.fullmatch(tag) is None or tag in tags:
            common.fail("record-chain-invalid:tag")
        target = ref["object"]
        if target.get("type") != "commit":
            common.fail("record-chain-invalid:tag")
        tags[tag] = common.require_sha1(target.get("sha"), "record-chain-invalid:tag")
    if set(tags) != set(published):
        common.fail("record-chain-invalid:tag-release-parity")
    if len(published) > MAX_RECORDS:
        common.fail("record-chain-invalid:too-many")
    entries: list[Entry] = [bootstrap_entry(bootstrap)]
    for index, tag in enumerate(sorted(published)):
        directory = work_dir / f"record-{index}"
        try:
            raw = gh.download_manifest(tag, directory)
        except common.ReleaseError as exc:
            raise common.ReleaseError("record-chain-invalid:download") from exc
        if len(raw) != published[tag]["assets"][0]["size"]:
            common.fail("record-chain-invalid:asset-size")
        try:
            manifest = validate_manifest(common.loads_strict(raw, code="record-invalid"))
        except common.ReleaseError as exc:
            raise common.ReleaseError("record-chain-invalid:manifest") from exc
        if raw != common.canonical_file_bytes(manifest):
            common.fail("record-chain-invalid:manifest")
        if manifest["release"] != tag or tags[tag] != manifest["sha"]:
            common.fail("record-chain-invalid:tag-binding")
        workflow_sha = manifest["signer"]["workflow_sha"]
        try:
            gh.verify_file(directory / MANIFEST_ASSET, signer_digest=workflow_sha, source_digest=workflow_sha)
        except common.ReleaseError as exc:
            raise common.ReleaseError("record-chain-invalid:attestation") from exc
        entry = manifest_entry(manifest, raw)
        require_link(entries, entry, repo_dir)
        entries.append(entry)
    return Chain(tuple(entries))


def verify_images(gh: Gh, images: Mapping[str, str], sha: str, bootstrap: Mapping[str, Any]) -> None:
    """Section 7.3. A digest equal to release #0's digest for the SAME
    component verifies with the pinned legacy signer; every other digest must
    verify with ship.yml at the record's own SHA."""
    target = spec_images.require_image_set(dict(images), "internal-error:images")
    common.require_sha1(sha, "internal-error:sha")
    legacy = bootstrap["legacy_signer"]
    for component in common.COMPONENTS:
        image = common.IMAGE_REPOSITORY[component]
        digest = target[component]
        if digest == bootstrap["images"][component]:
            gh.verify_image(
                image,
                digest,
                signer_workflow=legacy["workflow"],
                signer_digest=legacy["workflow_sha"],
                source_digest=legacy["workflow_sha"],
                predicate_type=SLSA_PREDICATE,
            )
            results = gh.verify_image(
                image,
                digest,
                signer_workflow=legacy["workflow"],
                signer_digest=legacy["workflow_sha"],
                source_digest=legacy["workflow_sha"],
                predicate_type=legacy["source_predicate"],
            )
            if not any(_legacy_predicate_matches(item, component, legacy) for item in results):
                common.fail("attestation-unverified:legacy-predicate")
        else:
            gh.verify_image(
                image,
                digest,
                signer_workflow=SHIP_SIGNER,
                signer_digest=sha,
                source_digest=sha,
                predicate_type=SLSA_PREDICATE,
            )


def _legacy_predicate_matches(item: Mapping[str, Any], component: str, legacy: Mapping[str, Any]) -> bool:
    """The fields build-attest-exact-release-images.yml:2284-2299 binds."""
    try:
        predicate = item["verificationResult"]["statement"]["predicate"]
        return (
            predicate["image"]["component"] == component
            and predicate["source"]["commit"] == legacy["source_commit"]
            and predicate["phase"] == legacy["phase"]
            and predicate["builder"]["workflow_sha"] == legacy["workflow_sha"]
            and predicate["image"]["tag_is_authority"] is False
        )
    except (KeyError, TypeError):
        return False


# --------------------------------------------------------------------------
# Rollback planning
# --------------------------------------------------------------------------


def rollback_plan(chain: Chain, tag: str) -> tuple[Entry, tuple[Entry, ...]]:
    """Target = prod-0000 or a chain record (never newer than latest; the
    latest itself means restore intent) plus every record after it."""
    if tag != common.BOOTSTRAP_TAG and common.RECORD_TAG_RE.fullmatch(tag or "") is None:
        common.fail("rollback-target-invalid")
    index = chain.find(tag)
    if index is None:
        common.fail("rollback-target-invalid:unknown")
    target = chain.entries[index]
    intermediates = chain.entries[index + 1:]
    if any(entry.db_change for entry in intermediates):
        common.fail("rollback-target-invalid:db-change")
    return target, intermediates


def decide_rollback_case(live: Mapping[str, str], latest: Entry, target: Entry) -> str:
    live_images = dict(live)
    if target.release == latest.release:
        if live_images == latest.images:
            common.fail("nothing-to-roll-back")
        return "restore"
    if live_images == target.images:
        return "reconcile"
    if live_images == latest.images:
        return "rollback"
    common.fail("drift")
