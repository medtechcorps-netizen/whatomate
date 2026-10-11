"""Authenticate one compatibility artifact; collection and ship wiring follow.

Run metadata is a contract with a future trusted Actions collector. This module
does not enumerate runs, establish latest-release freshness or authorize a
release. It constructs subject receipts only after invoking the pinned Gh
verifier, then applies the pure canonical pair validator.
"""
from dataclasses import dataclass
import hashlib
import io
from pathlib import Path
import stat
import tempfile
import zipfile
import zlib

import compat_evidence as evidence
import release_record as records
import ship_common as common

MAX_ARCHIVE = 64 * 1024
MAX_VERIFICATION = 2 * 1024 * 1024
FILES = {"compat-result.json": 16384, "compat-context.json": 4096}


@dataclass(frozen=True, slots=True)
class ArtifactLocation:
    """API metadata, not a caller assertion that signatures were verified."""
    artifact_id: int
    name: str
    run_id: int
    run_attempt: int
    sha: str
    expired: bool


def _need(ok, detail):
    if not ok:
        raise evidence.CompatRefusal("compat-stale", detail)


def _digest(raw):
    return hashlib.sha256(raw).hexdigest()


def _identity(location, attempt):
    _need(type(attempt) is evidence.Attempt, "adapter-attempt")
    _need(type(attempt.sha) is str and common.SHA1_RE.fullmatch(attempt.sha) is not None,
          "adapter-attempt")
    _need(attempt.repository == evidence.REPOSITORY and attempt.workflow_path == evidence.WORKFLOW
          and attempt.workflow_sha == attempt.sha and attempt.ref == evidence.REF
          and attempt.event in ("push", "workflow_dispatch"), "adapter-attempt")
    _need(all(type(n) is int and 1 <= n < 10**16 for n in
              (attempt.run_id, attempt.run_number))
          and type(attempt.run_attempt) is int and 1 <= attempt.run_attempt <= 9999, "adapter-attempt")
    _need(type(location) is ArtifactLocation and type(location.artifact_id) is int
          and 1 <= location.artifact_id < 10**16 and location.expired is False
          and location.name == f"compat-result-{attempt.sha}-{attempt.run_attempt}"
          and location.sha == attempt.sha and type(location.run_id) is int
          and location.run_id == attempt.run_id and type(location.run_attempt) is int
          and location.run_attempt == attempt.run_attempt, "adapter-artifact")


def _archive(raw):
    _need(type(raw) is bytes and 0 < len(raw) <= MAX_ARCHIVE, "adapter-archive")
    try:
        with zipfile.ZipFile(io.BytesIO(raw)) as archive:
            entries = archive.infolist()
            _need(len(entries) == 2 and {entry.filename for entry in entries} == set(FILES),
                  "adapter-inventory")
            files = {}
            for entry in entries:
                mode = entry.external_attr >> 16
                _need(entry.orig_filename == entry.filename
                      and not entry.is_dir() and not (entry.flag_bits & 1)
                      and entry.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)
                      and stat.S_IFMT(mode) in (0, stat.S_IFREG)
                      and not (entry.external_attr & 0x10)
                      and 0 < entry.file_size <= FILES[entry.filename], "adapter-entry")
                with archive.open(entry) as handle:
                    body = handle.read(FILES[entry.filename] + 1)
                _need(len(body) == entry.file_size and len(body) <= FILES[entry.filename], "adapter-entry")
                files[entry.filename] = body
            return files
    except (zipfile.BadZipFile, NotImplementedError, RuntimeError, OSError, ValueError, EOFError, zlib.error):
        raise evidence.CompatRefusal("compat-stale", "adapter-archive") from None


def _directory(path):
    _need(isinstance(path, Path) and path.is_absolute(), "adapter-scratch")
    for parent in (path, *path.parents):
        _need(not parent.is_symlink() and not getattr(parent, "is_junction", lambda: False)()
              and parent.is_dir(), "adapter-scratch")


def _unchanged(directory, files, identities):
    _directory(directory)
    _need({p.name for p in directory.iterdir()} == set(FILES), "adapter-file-changed")
    for name, raw in files.items():
        path = directory / name
        info = path.lstat()
        _need(stat.S_ISREG(info.st_mode) and info.st_nlink == 1
              and not path.is_symlink() and not getattr(path, "is_junction", lambda: False)()
              and (info.st_dev, info.st_ino) == identities[name]
              and info.st_size == len(raw), "adapter-file-changed")
        with path.open("rb") as handle:
            _need(handle.read(len(raw) + 1) == raw, "adapter-file-changed")


def _subject(gh, path, raw, attempt):
    identity = gh.signer_identity(evidence.WORKFLOW, attempt.sha, attempt.sha)
    flags = gh.signer_flags(evidence.WORKFLOW, attempt.sha, attempt.sha, evidence.PREDICATE)
    flags += ["--cert-oidc-issuer", evidence.ISSUER]
    verified = gh._verify(str(path), flags, identity, code="attestation-unverified:compat")
    _need(type(verified) is list and 1 <= len(verified) <= 30, "adapter-attestation")
    transcript = common.canonical_file_bytes(verified)
    _need(len(transcript) <= MAX_VERIFICATION, "adapter-attestation")
    invocation = f"https://github.com/{evidence.REPOSITORY}/actions/runs/{attempt.run_id}/attempts/{attempt.run_attempt}"
    repository = f"https://github.com/{evidence.REPOSITORY}"
    for item in verified:
        _need(type(item) is dict and records.certificate_matches(item, identity), "adapter-certificate")
        result = item["verificationResult"]
        certificate = result["signature"]["certificate"]
        expected = {"issuer": evidence.ISSUER, "runInvocationURI": invocation,
                    "sourceRepositoryURI": repository, "buildTrigger": attempt.event}
        _need(all(type(certificate.get(key)) is str and certificate[key] == value
                  for key, value in expected.items()), "adapter-certificate")
        statement = result["statement"]
        _need(type(statement) is dict and statement.get("_type") == "https://in-toto.io/Statement/v1"
              and statement.get("predicateType") == evidence.PREDICATE
              and statement.get("subject") == [{"name": path.name, "digest": {"sha256": _digest(raw)}}],
              "adapter-subject")
        predicate = statement["predicate"]
        definition, details = predicate["buildDefinition"], predicate["runDetails"]
        _need(definition["buildType"] == "https://actions.github.io/buildtypes/workflow/v1"
              and definition["externalParameters"]["workflow"] == {
                  "path": evidence.WORKFLOW, "ref": evidence.REF, "repository": repository}
              and definition["resolvedDependencies"] == [{"digest": {"gitCommit": attempt.sha},
                  "uri": f"git+{repository}@{evidence.REF}"}]
              and details["builder"]["id"] == evidence.SIGNER
              and details["metadata"]["invocationId"] == invocation, "adapter-provenance")
    return evidence.VerifiedSubject(path.name, _digest(raw), evidence.PREDICATE, evidence.ISSUER,
        evidence.SIGNER, attempt.sha, attempt.sha, evidence.REF, "github-hosted",
        attempt.run_id, attempt.run_attempt, _digest(transcript))


def verify_pair(gh: records.Gh, archive: bytes, location: ArtifactLocation,
                attempt: evidence.Attempt, scratch: Path) -> evidence.AuthenticatedPair:
    """Verify both actual files; a valid negative pair is returned as negative.

    Only a reviewed Actions metadata collector may establish ``location`` and
    ``attempt``. This API accepts no preverified JSON transcript or boolean.
    It does not turn the artifact into a successful qualification decision.
    """
    try:
        _identity(location, attempt)
        _need(gh.repo == evidence.REPOSITORY, "adapter-repository")
        files = _archive(archive)
        _directory(scratch)
        with tempfile.TemporaryDirectory(prefix="compat-verify-", dir=scratch) as temporary:
            directory = Path(temporary)
            identities = {}
            for name, raw in files.items():
                path = directory / name
                with path.open("xb") as handle:
                    handle.write(raw)
                info = path.lstat()
                identities[name] = (info.st_dev, info.st_ino)
            _unchanged(directory, files, identities)
            subjects = {}
            for name in FILES:
                subjects[name] = _subject(gh, directory / name, files[name], attempt)
                _unchanged(directory, files, identities)
            artifact = evidence.Artifact(location.artifact_id, location.name, location.run_id,
                location.run_attempt, location.sha, False,
                tuple(evidence.ArtifactFile(name, _digest(files[name])) for name in FILES))
            pair = evidence.AuthenticatedPair(files["compat-result.json"], files["compat-context.json"],
                subjects["compat-result.json"], subjects["compat-context.json"], artifact)
            evidence.parse_authenticated_pair(pair, attempt)
            return pair
    except evidence.CompatRefusal:
        raise
    except (common.ReleaseError, OSError, ValueError, TypeError, KeyError, AttributeError,
            RecursionError, OverflowError):
        raise evidence.CompatRefusal("compat-stale", "adapter-verification") from None
