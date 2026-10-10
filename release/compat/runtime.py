"""Bounded CLI building blocks, not the compatibility experiment runner.

No command executes at import. Callers supply reviewed executable paths and
fresh empty CLI/scratch directories. Raw captured bytes are private working
data, never a public result; errors/repr never contain command or stream data.
Docker exposes only pinned pulls/inspection. Container ownership, cleanup,
ordered SQL/log capture, O-1/overlay, sweeps and publication remain unimplemented.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import threading
import time
from typing import Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import proof
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "deployment"))
import release_record
import ship_common as common

MIB = 1024 * 1024
MAX_STREAM = 64 * MIB
MAX_SECONDS = 600
POSTGRES = "postgres:17@sha256:e38411452a464af89e5adadb8d223bf53b898d47d6ef918b2d58c08707350449"
REDIS = "redis:7@sha256:91d0f7e8c748ec7a4c2b4fb2c4f84edab794dd91d01e095e38dc906db9d684ab"
_WEB = re.compile(re.escape(common.IMAGE_REPOSITORY["web"]) + r"@sha256:[0-9a-f]{64}\Z")
_REPO_API = "/repos/" + common.REPOSITORY


def _need(ok: bool) -> None:
    if not ok:
        raise proof.EvidenceError("runtime-input-invalid")


@dataclass(frozen=True)
class Capture:
    """A completed command, not a semantic verdict. Nonzero exits remain data."""
    returncode: int
    stdout: bytes = field(repr=False)
    stderr: bytes = field(repr=False)


def bounded_command(
    argv: Sequence[str], *, env: Mapping[str, str], timeout: float,
    stdout_limit: int, stderr_limit: int = MIB,
) -> Capture:
    """Drain both pipes concurrently; cap bytes while reading, deadline and reap.

    POSIX commands have a private process group killed on any capture failure.
    Windows kills/reaps the direct child; no Windows process-tree guarantee is
    made. Reader threads are daemonized and never allowed to delay the bound
    when an unrelated inherited pipe remains open. Docker daemon resources
    are NOT cleaned up by killing the CLI; the later runner must own cleanup.
    """
    _need(type(argv) in (list, tuple) and 0 < len(argv) <= 256)
    _need(all(type(arg) is str and len(arg) <= 8192 and not any(c in arg for c in "\0\r\n") for arg in argv))
    _need(bool(argv[0]) and type(env) is dict and len(env) <= 128)
    _need(all(type(k) is str and type(v) is str and k and "=" not in k and not any(c in k + v for c in "\0\r\n") for k, v in env.items()))
    _need(type(timeout) in (int, float) and math.isfinite(timeout) and 0 < timeout <= MAX_SECONDS)
    _need(all(type(n) is int and 0 <= n <= MAX_STREAM for n in (stdout_limit, stderr_limit)))
    deadline = time.monotonic() + timeout
    try:
        proc = subprocess.Popen(list(argv), env=dict(env), stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                shell=False, start_new_session=os.name == "posix")
    except (OSError, ValueError, subprocess.SubprocessError):
        raise proof.InfrastructureFailure("process") from None
    streams = (proc.stdout, proc.stderr)
    chunks = [bytearray(), bytearray()]
    failed = threading.Event()

    def drain(index: int, limit: int) -> None:
        stream = streams[index]
        try:
            while True:
                # Read at most one byte beyond the cap; retain no excess data.
                part = stream.read(min(65536, limit - len(chunks[index]) + 1))
                if not part:
                    break
                if len(chunks[index]) + len(part) > limit:
                    failed.set()
                    break
                chunks[index].extend(part)
        except (OSError, ValueError):
            failed.set()
        finally:
            try:
                stream.close()
            except OSError:
                failed.set()

    threads = [threading.Thread(target=drain, args=(i, cap), daemon=True)
               for i, cap in enumerate((stdout_limit, stderr_limit))]

    def kill() -> None:
        try:
            if os.name == "posix":
                os.killpg(proc.pid, signal.SIGKILL)
            else:
                proc.kill()
        except (ProcessLookupError, OSError):
            pass

    try:
        try:
            for thread in threads:
                thread.start()
        except RuntimeError:
            raise proof.InfrastructureFailure("process") from None
        while proc.poll() is None or any(thread.is_alive() for thread in threads):
            if failed.is_set() or time.monotonic() >= deadline:
                raise proof.InfrastructureFailure("capture")
            time.sleep(min(0.01, max(0, deadline - time.monotonic())))
        if failed.is_set():
            raise proof.InfrastructureFailure("capture")
        return Capture(proc.returncode, bytes(chunks[0]), bytes(chunks[1]))
    finally:
        if proc.poll() is None or any(thread.is_alive() for thread in threads):
            kill()
        try:
            proc.wait(timeout=2)
        except (OSError, subprocess.SubprocessError):
            raise proof.InfrastructureFailure("process") from None
        for thread in threads:
            if thread.ident is not None:
                thread.join(timeout=0.1)
        for thread, stream in zip(threads, streams):
            if thread.ident is None:
                try:
                    stream.close()
                except OSError:
                    raise proof.InfrastructureFailure("capture") from None


def _empty_directory(value: Path) -> Path:
    _need(isinstance(value, Path) and value.is_absolute())
    try:
        _need(value.resolve() == value and value.is_dir() and not value.is_symlink() and not any(value.iterdir()))
        return value.resolve()
    except OSError:
        raise proof.InfrastructureFailure("capture") from None


def _binary(value: str) -> str:
    _need(type(value) is str and value and not any(c in value for c in "\0\r\n"))
    return value


class Docker:
    """Anonymous pulls and inspect only, with an explicitly empty Docker config.

    Uses the default local context. No host/context overrides, login, builds,
    container mutation, credential mounts or arbitrary argument passthrough.
    Infrastructure pins match existing Part A workflows. The previous web
    digest must come from verified_previous(), not an arbitrary mutable tag.
    """
    def __init__(self, binary: str, config_dir: Path, *, base_env=None, capture=bounded_command):
        self.binary = _binary(binary)
        self.config = _empty_directory(config_dir)
        self.env = common.scrubbed_env(base={} if base_env is None else base_env)
        self.capture = capture

    @staticmethod
    def _image(image: str) -> None:
        _need(type(image) is str and (image in (POSTGRES, REDIS) or _WEB.fullmatch(image) is not None))

    def _call(self, args: list[str], failure: str) -> Capture:
        result = self.capture([self.binary, "--config", str(self.config), "--context", "default", *args],
                              env=dict(self.env), timeout=180, stdout_limit=MIB, stderr_limit=MIB)
        if result.returncode != 0:
            raise proof.InfrastructureFailure(failure)
        return result

    def pull(self, image: str) -> None:
        self._image(image)
        self._call(["image", "pull", "--platform", "linux/amd64", image], "pull")

    def inspect(self, image: str) -> dict:
        self._image(image)
        raw = self._call(["image", "inspect", image], "container").stdout
        try:
            value = common.loads_strict(raw)
            _need(type(value) is list and len(value) == 1 and type(value[0]) is dict)
            item = value[0]
            _need(item.get("Os") == "linux" and item.get("Architecture") == "amd64")
            # Docker normalizes familiar names and strips the tag in RepoDigests.
            wanted = image.split("@", 1)[1]
            repository = common.IMAGE_REPOSITORY["web"] if _WEB.fullmatch(image) else ("postgres" if image == POSTGRES else "redis")
            permitted = {repository + "@" + wanted}
            if repository in ("postgres", "redis"):
                permitted.add("docker.io/library/" + repository + "@" + wanted)
            _need(type(item.get("RepoDigests")) is list and any(x in permitted for x in item["RepoDigests"]))
            return item
        except (common.ReleaseError, proof.EvidenceError, TypeError):
            raise proof.InfrastructureFailure("capture") from None


class ReadOnlyGh(release_record.Gh):
    """Reuse release trust policy with bounded read-only CLI operations.

    The future hosted caller supplies its existing read-only job token in
    memory. This module never retrieves credentials. Empty config/work dirs
    isolate local CLI state. There are no release/API writes or auth commands.
    """
    def __init__(self, binary: str, token: str, *, expected_version: str,
                 config_dir: Path, work_dir: Path, base_env=None, capture=bounded_command):
        self.capture = capture
        self.work_dir = _empty_directory(work_dir)
        config = _empty_directory(config_dir)
        _need(config != self.work_dir)
        self.assets: dict[str, tuple[int, int]] = {}
        self.downloaded: dict[Path, bytes] = {}
        super().__init__(_binary(binary), token, common.REPOSITORY, expected_version=expected_version,
                         config_dir=str(config), base_env={} if base_env is None else base_env)
        self._env["GH_HOST"] = "github.com"

    def _run(self, args, *, code):
        args = list(args)
        allowed = args == ["--version"]
        allowed |= args in (["api", "--paginate", "--slurp", _REPO_API + "/releases?per_page=100"],
                            ["api", "--paginate", "--slurp", _REPO_API + "/git/matching-refs/tags/prod-"])
        asset = any(args == ["api", _REPO_API + f"/releases/assets/{identity}", "-H", "Accept: application/octet-stream"]
                    for identity, _ in self.assets.values())
        # Only the inherited signer_flags/verify_file/verify_image entrypoints
        # create attestation arguments; reject arbitrary direct _run callers.
        allowed |= asset or args == getattr(self, "_verification_args", None)
        if not allowed:
            raise proof.EvidenceError("gh-read-command-refused")
        limit = release_record.MAX_MANIFEST_BYTES if asset else release_record.MAX_GH_OUTPUT_BYTES
        result = self.capture([self.gh_bin, *args], env=dict(self._env), timeout=180,
                              stdout_limit=limit, stderr_limit=MIB)
        if result.returncode != 0:
            raise proof.InfrastructureFailure("network")
        return result.stdout

    def _verify(self, subject, flags, identity, *, code):
        # Preserve the inherited exact certificate parser and command builder.
        if getattr(self, "_verification_args", None) is not None:
            raise proof.EvidenceError("gh-read-command-refused")
        self._verification_args = ["attestation", "verify", subject, *flags, "--format", "json"]
        try:
            return super()._verify(subject, flags, identity, code=code)
        finally:
            self._verification_args = None

    def releases(self):
        values = super().releases()
        self.assets = {}
        for value in values:
            if type(value) is not dict:
                continue  # resolve_chain supplies the definitive shape refusal.
            tag = value.get("tag_name")
            if type(tag) is not str or common.RECORD_TAG_RE.fullmatch(tag) is None or value.get("draft") is not False:
                continue
            assets = value.get("assets")
            if type(assets) is list and len(assets) == 1 and type(assets[0]) is dict:
                item = assets[0]
                if (item.get("name") == release_record.MANIFEST_ASSET and type(item.get("id")) is int
                        and item["id"] > 0 and type(item.get("size")) is int
                        and 0 < item["size"] <= release_record.MAX_MANIFEST_BYTES):
                    self.assets[tag] = (item["id"], item["size"])
        return values

    def download_manifest(self, tag: str, directory: Path) -> bytes:
        _need(type(tag) is str and common.RECORD_TAG_RE.fullmatch(tag) is not None and tag in self.assets)
        _need(isinstance(directory, Path))
        try:
            _need(directory.parent.resolve() == self.work_dir
                  and re.fullmatch(r"record-[0-9]+", directory.name) is not None
                  and not directory.exists() and not directory.is_symlink())
        except OSError:
            raise proof.InfrastructureFailure("capture") from None
        identity, size = self.assets[tag]
        raw = self._run(["api", _REPO_API + f"/releases/assets/{identity}", "-H", "Accept: application/octet-stream"], code="gh-failed:download")
        _need(type(raw) is bytes and len(raw) == size)
        try:
            directory.mkdir()
            with (directory / release_record.MANIFEST_ASSET).open("xb") as stream:
                stream.write(raw)
        except OSError:
            raise proof.InfrastructureFailure("capture") from None
        self.downloaded[directory / release_record.MANIFEST_ASSET] = raw
        return raw

    def verify_file(self, path: Path, *, signer_digest: str, source_digest: str):
        # The chain only verifies this adapter's just-downloaded manifest.
        # Do not expose the inherited arbitrary-local-file read capability.
        _need(isinstance(path, Path) and path in self.downloaded)
        def current() -> bytes:
            try:
                _need(path.resolve() == path and not path.is_symlink())
                with path.open("rb") as stream:
                    return stream.read(release_record.MAX_MANIFEST_BYTES + 1)
            except OSError:
                raise proof.InfrastructureFailure("capture") from None
        _need(current() == self.downloaded[path])
        result = super().verify_file(path, signer_digest=signer_digest, source_digest=source_digest)
        _need(current() == self.downloaded[path])
        return result

    def run(self, args, *, code):
        raise proof.EvidenceError("gh-write-refused")

    def verify_image(self, *args, **kwargs):
        # resolve_chain/verify_images never needs an arbitrary local bundle.
        _need(kwargs.get("bundle") is None)
        return super().verify_image(*args, **kwargs)


def verified_previous(client: ReadOnlyGh, repo_dir: Path, *, rollback_target: str | None = None) -> release_record.Entry:
    """Resolve exact Part A chain/signatures; optionally select a validated tag.

    Reuses all three image verifications, including the bootstrap legacy
    predicate rule, instead of cloning a web-only trust policy. Local Git
    ancestry remains the existing release_record implementation; it is not a
    network operation or a replacement classifier. No compatibility flag is
    returned on any incomplete or invalid release evidence.
    """
    _need(isinstance(client, ReadOnlyGh) and isinstance(repo_dir, Path) and repo_dir.is_dir())
    if rollback_target is not None:
        proof.validate_rollback_target(rollback_target)
    try:
        bootstrap = release_record.load_bootstrap()
        chain = release_record.resolve_chain(client, repo_dir, bootstrap=bootstrap, work_dir=client.work_dir)
        index = None if rollback_target is None else chain.find(rollback_target)
        if rollback_target is not None and index is None:
            raise proof.EvidenceError("rollback-target-missing")
        selected = chain.latest if index is None else chain.entries[index]
        release_record.verify_images(client, selected.images, selected.sha, bootstrap)
        return selected
    except common.ReleaseError:
        raise proof.EvidenceError("release-verification-failed") from None
