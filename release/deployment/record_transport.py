"""Preparatory bounded release reads; no caller or writer activation.

The client authenticates owned wire bytes and selected images using the existing
release verifier. It does not authorize source policy or deployment. One caller
deadline covers GitHub and immutable Git capture; no command runs at import.
"""
from dataclasses import dataclass
import math
from pathlib import Path
import re
import sys
import threading
import time

import release_record as records
import ship_common as common

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "compat"))
import runtime

API = "/repos/" + common.REPOSITORY
REPOSITORY_ID = 1297344686
HEADERS = ("-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28")
MIB = 1024 * 1024
MAX_COMMANDS = 6000
MAX_BYTES = 128 * MIB
PAGE_LIMIT = 20
CLEANUP_RESERVE = 2.25
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")



# Fixed query documents only; callers cannot supply GraphQL source or endpoints.
_CONTRACT_QUERIES = {
    'associations': 'query ContractAssociations($owner: String!, $name: String!, $candidate: GitObjectID!) {\n  repository(owner: $owner, name: $name) {\n    databaseId\n    nameWithOwner\n    owner { __typename login ... on User { id } }\n    defaultBranchRef { name target { oid } }\n    object(oid: $candidate) { __typename ... on Commit {\n      oid\n      associatedPullRequests(first: 100) {\n        pageInfo { hasNextPage endCursor }\n        edges { cursor node { id number merged isDraft state baseRefName\n      mergeCommit { oid } headRefOid mergedAt\n      repository { databaseId nameWithOwner } } }\n      }\n    } }\n  }\n}\n',
    'labels': 'query ContractLabels($owner: String!, $name: String!, $candidate: GitObjectID!, $number: Int!) {\n  repository(owner: $owner, name: $name) {\n    databaseId\n    nameWithOwner\n    owner { __typename login ... on User { id } }\n    defaultBranchRef { name target { oid } }\n    object(oid: $candidate) { __typename ... on Commit { oid } }\n    pullRequest(number: $number) {\n      id number merged isDraft state baseRefName\n      mergeCommit { oid } headRefOid mergedAt\n      repository { databaseId nameWithOwner }\n      labels(first: 100) {\n        pageInfo { hasNextPage endCursor }\n        edges { cursor node { id name createdAt updatedAt repository { databaseId nameWithOwner } } }\n      }\n    }\n  }\n}\n',
    'events': 'query ContractEvents($owner: String!, $name: String!, $candidate: GitObjectID!, $number: Int!, $after: String) {\n  repository(owner: $owner, name: $name) {\n    databaseId\n    nameWithOwner\n    owner { __typename login ... on User { id } }\n    defaultBranchRef { name target { oid } }\n    object(oid: $candidate) { __typename ... on Commit { oid } }\n    pullRequest(number: $number) {\n      id number merged isDraft state baseRefName\n      mergeCommit { oid } headRefOid mergedAt\n      repository { databaseId nameWithOwner }\n      timelineItems(first: 100, after: $after, itemTypes: [LABELED_EVENT, UNLABELED_EVENT]) {\n        pageInfo { hasNextPage endCursor }\n        edges { cursor node {\n          __typename\n          ... on LabeledEvent { id createdAt actor { __typename login ... on User { id } ... on Bot { id } ... on Organization { id } }\n            label { id name createdAt updatedAt repository { databaseId nameWithOwner } }\n            labelable { __typename ... on PullRequest { id repository { databaseId nameWithOwner } } } }\n          ... on UnlabeledEvent { id createdAt actor { __typename login ... on User { id } ... on Bot { id } ... on Organization { id } }\n            label { id name createdAt updatedAt repository { databaseId nameWithOwner } }\n            labelable { __typename ... on PullRequest { id repository { databaseId nameWithOwner } } } }\n        } }\n      }\n    }\n  }\n}\n',
}
_CONTRACT_STDERR = 65536
_CONTRACT_BYTES = 8 * MIB


class TransportRefusal(common.ReleaseError):
    pass


def _failure(detail):
    return TransportRefusal("record-chain-invalid:transport-" + detail)


def _finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def _integer(value):
    return type(value) is int and 0 < value < 10**16


class OperationBudget:
    """One deadline and summed capture allowance, including caller-owned Git.

    capture/clock are private testing seams. Passing capture to ImmutableGit
    does not widen that module's command grammar. A failed operation cannot
    acquire a new allowance by creating another client around this budget.
    """
    def __init__(self, deadline, *, capture=runtime.bounded_command, clock=time.monotonic):
        now = clock()
        if not _finite(now) or not _finite(deadline) or deadline <= now:
            raise _failure("deadline")
        self._clock, self._capture = clock, capture
        self._last = now
        self._deadline = min(deadline, now + 600)
        self.commands = self.captured_bytes = 0
        self.failed = self.closed = False
        self._lock = threading.Lock()

    @property
    def clock(self):
        return self._clock

    def fail(self, detail):
        self.failed = True
        raise _failure(detail) from None

    def remaining(self):
        if self.failed or self.closed:
            self.fail("closed")
        now = self._clock()
        if not _finite(now) or not self._last <= now < self._deadline:
            self.fail("deadline")
        self._last = now
        return self._deadline - now

    def check(self):
        self.remaining()

    def close(self):
        if self.closed:
            return
        try:
            if not self.failed:
                self.check()
        finally:
            self.closed = True

    def capture(self, argv, *, env, timeout, stdout_limit, stderr_limit):
        remaining = self.remaining()
        if (not _finite(timeout) or timeout <= 0 or not all(type(v) is int and 0 <= v <= 64 * MIB
                for v in (stdout_limit, stderr_limit))):
            self.fail("capture-input")
        if not self._lock.acquire(blocking=False):
            self.fail("concurrent")
        try:
            allowance = MAX_BYTES - self.captured_bytes
            if self.commands >= MAX_COMMANDS or allowance <= stderr_limit:
                self.fail("bound")
            duration = min(timeout, remaining - CLEANUP_RESERVE)
            if duration <= 0:
                self.fail("deadline")
            limit = min(stdout_limit, allowance - stderr_limit)
            self.commands += 1
            try:
                result = self._capture(argv, env=env, timeout=duration,
                                       stdout_limit=limit, stderr_limit=stderr_limit)
            except Exception:
                self.fail("capture")
            if (type(result) is not runtime.Capture or type(result.returncode) is not int
                    or type(result.stdout) is not bytes or type(result.stderr) is not bytes
                    or len(result.stdout) > limit or len(result.stderr) > stderr_limit):
                self.fail("capture")
            self.captured_bytes += len(result.stdout) + len(result.stderr)
            self.check()
            return result
        finally:
            self._lock.release()


@dataclass(frozen=True, slots=True)
class ReleaseIdentity:
    release: str
    release_id: int
    asset_id: int
    size: int
    digest: str | None


@dataclass(frozen=True, slots=True)
class TagIdentity:
    release: str
    sha: str


@dataclass(frozen=True, slots=True)
class InventorySnapshot:
    repository_id: int
    main_sha: str
    records: tuple[ReleaseIdentity, ...]
    tags: tuple[TagIdentity, ...]


@dataclass(frozen=True, slots=True)
class VerifiedImages:
    release: str
    record_sha: str
    images: tuple[tuple[str, str], ...]
    source_shas: tuple[tuple[str, str], ...]


def _directory(path, *, empty=False):
    if not isinstance(path, Path) or not path.is_absolute():
        raise _failure("directory")
    for part in (path, *path.parents):
        if (not part.is_dir() or part.is_symlink()
                or getattr(part, "is_junction", lambda: False)()):
            raise _failure("directory")
    if empty and any(path.iterdir()):
        raise _failure("directory")
    value = path.stat()
    return value.st_dev, value.st_ino


class ReleaseGh(records.Gh):
    """Fixed GETs, owned manifests and inherited exact signature predicates."""
    def __init__(self, binary, token, config_dir, work_dir, *, budget, base_env=None):
        if type(budget) is not OperationBudget:
            raise _failure("budget")
        self.budget = budget
        self.work_dir, self._config = work_dir, config_dir
        self._snapshot = None
        self._releases = b""
        self._downloads = {}
        self._verified = {}
        self._download_attempts = set()
        self._verification_args = self._scope = self._image_scope = None
        self._finished = False
        self._contract_request = None
        self._contract_collected = False
        try:
            self._work_identity = _directory(work_dir, empty=True)
            self._config_identity = _directory(config_dir, empty=True)
            self._need(work_dir.resolve() != config_dir.resolve(), "directory")
            self._need(isinstance(binary, Path) and binary.is_absolute() and binary.is_file(), "binary")
            super().__init__(str(binary), token, common.REPOSITORY, expected_version="2.98.0",
                             config_dir=str(config_dir), base_env=base_env)
        except (OSError, ValueError, common.ReleaseError):
            self.budget.fail("client")

    def _need(self, ok, detail):
        if not ok:
            self.budget.fail(detail)

    def _check(self):
        self.budget.check()
        self._need(not self._finished, "closed")
        try:
            self._need(_directory(self.work_dir) == self._work_identity
                       and _directory(self._config) == self._config_identity, "directory-changed")
        except (OSError, common.ReleaseError):
            self.budget.fail("directory-changed")

    def _run(self, args, *, code):
        del code
        self._check()
        args = list(args)
        limit, duration = 4 * MIB, 30
        allowed = args == ["--version"]
        if allowed:
            duration = 5
        if len(args) == 8 and args[:3] == ["api", "--method", "GET"] and tuple(args[4:]) == HEADERS:
            path = args[3]
            allowed = path in (API, API + "/git/ref/heads/main")
            allowed |= re.fullmatch(re.escape(API) + r"/(releases|tags)\?per_page=100&page=([1-9]|1[0-9]|20)", path) is not None
            # Per-tag reads are admitted only while iterating exact inventory
            # names or rechecking the already-bound selected/latest snapshot.
            allowed |= path in getattr(self, "_tag_gets", set())
        if (len(args) == 7 and args[:3] == ["api", "--method", "GET"]
                and args[4:] == ["-H", "Accept: application/octet-stream", "--allow-escape-sequences"]):
            identities = {row.asset_id for row in self._snapshot.records} if self._snapshot else set()
            allowed = any(args[3] == API + f"/releases/assets/{identity}" for identity in identities)
            limit = records.MAX_MANIFEST_BYTES
        if self._scope is not None and args == self._verification_args:
            allowed, duration = True, 60
        contract = self._contract_request
        if contract is not None:
            self._need(tuple(args) == contract[0], "contract-scope")
            allowed, limit = True, contract[1]
        self._need(allowed, "command")
        env = dict(self._env)
        env["GH_HOST"] = "github.com"
        result = self.budget.capture([self.gh_bin, *args], env=env, timeout=duration,
                                     stdout_limit=limit, stderr_limit=65536)
        self._check()
        self._need(result.returncode == 0, "command-failed")
        if contract is not None:
            self._need(not result.stderr, "contract-response")
        return result.stdout

    def _contract_page(self, stage, candidate_sha, *, number=None, after=None, byte_allowance):
        """One fixed read query inside this client's original operation."""
        self._check()
        self._need(self._contract_request is None and self._scope is None
                   and self._image_scope is None, "contract-scope")
        self._need(type(self._snapshot) is InventorySnapshot
                   and self._snapshot.repository_id == REPOSITORY_ID
                   and type(candidate_sha) is str and _SHA.fullmatch(candidate_sha) is not None
                   and self._snapshot.main_sha == candidate_sha, "contract-source")
        self._need(type(stage) is str and stage in _CONTRACT_QUERIES, "contract-input")
        self._need(type(byte_allowance) is int
                   and _CONTRACT_STDERR < byte_allowance <= _CONTRACT_BYTES, "contract-bound")
        self._need((stage == "associations" and number is None and after is None)
                   or (stage != "associations" and type(number) is int and 0 < number < 2**31
                       and (stage == "events" or after is None)), "contract-input")
        self._need(after is None or (type(after) is str and 0 < len(after) <= 1024
                   and all(32 <= ord(char) < 127 for char in after)), "contract-input")
        owner, name = common.REPOSITORY.split("/")
        args = ["api", "--method", "POST", "graphql", *HEADERS,
                "-f", "query=" + _CONTRACT_QUERIES[stage],
                "-f", "owner=" + owner, "-f", "name=" + name,
                "-f", "candidate=" + candidate_sha]
        if number is not None:
            args.extend(["-F", "number=" + str(number)])
        if after is not None:
            args.extend(["-f", "after=" + after])
        scope = (tuple(args), min(MIB, byte_allowance - _CONTRACT_STDERR))
        self._contract_request = scope
        try:
            return self._run(args, code="record-chain-invalid:transport-contract")
        finally:
            self._need(self._contract_request is scope, "contract-scope")
            self._contract_request = None

    def _get(self, path):
        try:
            value = self._json(["api", "--method", "GET", path, *HEADERS], code="gh-failed:releases")
            self._check()
            return value
        except (common.ReleaseError, ValueError, TypeError, RecursionError):
            self.budget.fail("json")

    def _pages(self, endpoint, key):
        rows, seen = [], set()
        for page in range(1, PAGE_LIMIT + 1):
            values = self._get(API + f"/{endpoint}?per_page=100&page={page}")
            self._need(type(values) is list and len(values) <= 100, "page")
            for value in values:
                self._need(type(value) is dict, "page")
                identity = value.get(key)
                self._need(_integer(identity) if key == "id" else type(identity) is str and 0 < len(identity) <= 512, "page")
                self._need(identity not in seen, "duplicate")
                seen.add(identity)
                rows.append(value)
            if len(values) < 100:
                return rows
        self.budget.fail("page-bound")

    def _release(self, row):
        self._need(type(row) is dict and _integer(row.get("id")), "release")
        tag = row.get("tag_name")
        self._need(type(tag) is str, "release")
        if not tag.startswith("prod-") or tag == common.BOOTSTRAP_TAG or row.get("draft") is True:
            return None
        self._need(common.RECORD_TAG_RE.fullmatch(tag) is not None
                   and row.get("draft") is False and row.get("prerelease") is False, "release")
        assets = row.get("assets")
        self._need(type(assets) is list and len(assets) == 1 and type(assets[0]) is dict, "asset")
        asset = assets[0]
        self._need(_integer(asset.get("id")) and type(asset.get("size")) is int
                   and 0 < asset["size"] <= records.MAX_MANIFEST_BYTES
                   and asset.get("name") == records.MANIFEST_ASSET and asset.get("state") == "uploaded", "asset")
        digest = asset.get("digest")
        self._need(digest is None or (type(digest) is str and _DIGEST.fullmatch(digest) is not None), "asset")
        return ReleaseIdentity(tag, row["id"], asset["id"], asset["size"], digest)

    def _exact_ref(self, tag):
        path = API + "/git/ref/tags/" + tag
        self._tag_gets.add(path)
        value = self._get(path)
        self._need(type(value) is dict and value.get("ref") == "refs/tags/" + tag, "tag")
        obj = value.get("object")
        self._need(type(obj) is dict and obj.get("type") == "commit" and type(obj.get("sha")) is str
                   and _SHA.fullmatch(obj["sha"]) is not None, "tag")
        return TagIdentity(tag, obj["sha"])

    def _read_snapshot(self):
        self._tag_gets = set()
        repository = self._get(API)
        self._need(type(repository) is dict and type(repository.get("id")) is int
                   and repository["id"] == REPOSITORY_ID and repository.get("full_name") == common.REPOSITORY, "repository")
        main = self._get(API + "/git/ref/heads/main")
        self._need(type(main) is dict and main.get("ref") == "refs/heads/main"
                   and type(main.get("object")) is dict, "main")
        obj = main["object"]
        self._need(obj.get("type") == "commit" and type(obj.get("sha")) is str
                   and _SHA.fullmatch(obj["sha"]) is not None, "main")
        releases = self._pages("releases", "id")
        selected = tuple(item for row in releases if (item := self._release(row)) is not None)
        self._need(len(selected) <= records.MAX_RECORDS
                   and len({row.release for row in selected}) == len(selected)
                   and len({row.asset_id for row in selected}) == len(selected), "inventory")
        tags = []
        for row in self._pages("tags", "name"):
            name = row["name"]
            if not name.startswith("prod-") or name == common.BOOTSTRAP_TAG:
                continue
            self._need(common.RECORD_TAG_RE.fullmatch(name) is not None, "tag")
            self._need(type(row.get("commit")) is dict, "tag")
            exact = self._exact_ref(name)
            self._need(row["commit"].get("sha") == exact.sha, "tag")
            tags.append(exact)
        self._need({row.release for row in selected} == {row.release for row in tags}, "parity")
        snapshot = InventorySnapshot(REPOSITORY_ID, obj["sha"],
                                     tuple(sorted(selected, key=lambda x: x.release)),
                                     tuple(sorted(tags, key=lambda x: x.release)))
        self._check()
        # Retain only the collector's structural fields, not release bodies or
        # other unrelated API metadata. All relevant input checks ran above.
        projection = [{"id": row.release_id, "tag_name": row.release, "draft": False,
                       "prerelease": False, "assets": [{"id": row.asset_id,
                       "name": records.MANIFEST_ASSET, "size": row.size,
                       "state": "uploaded", "digest": row.digest}]} for row in snapshot.records]
        return snapshot, common.canonical_file_bytes(projection)

    def capture_snapshot(self):
        self._need(self._snapshot is None, "snapshot-repeat")
        self._snapshot, self._releases = self._read_snapshot()
        return self._snapshot

    def releases(self):
        self._check()
        if self._snapshot is None:
            self.capture_snapshot()
        return common.loads_strict(self._releases)

    def prod_tags(self):
        self._check()
        if self._snapshot is None:
            self.capture_snapshot()
        return [{"ref": "refs/tags/" + row.release, "object": {"type": "commit", "sha": row.sha}}
                for row in self._snapshot.tags]

    def download_manifest(self, tag, directory):
        self._check()
        rows = {row.release: row for row in self._snapshot.records} if self._snapshot else {}
        self._need(type(tag) is str and tag in rows and tag not in self._download_attempts, "download")
        self._need(isinstance(directory, Path) and directory.is_absolute() and directory.parent == self.work_dir
                   and re.fullmatch(r"record-[0-9]+", directory.name) is not None, "download-path")
        self._download_attempts.add(tag)
        try:
            self._need(not directory.exists() and not directory.is_symlink()
                       and not getattr(directory, "is_junction", lambda: False)(), "download-path")
            directory.mkdir()
            directory_id = _directory(directory, empty=True)
            row = rows[tag]
            raw = self._run(["api", "--method", "GET", API + f"/releases/assets/{row.asset_id}",
                             "-H", "Accept: application/octet-stream", "--allow-escape-sequences"], code="gh-failed:download")
            self._need(len(raw) == row.size and (row.digest is None or row.digest == "sha256:" + common.sha256_bytes(raw)), "download-bytes")
            self._need(_directory(directory) == directory_id, "download-path")
            path = directory / records.MANIFEST_ASSET
            with path.open("xb") as stream:
                stream.write(raw)
            self._downloads[path] = (tag, raw, directory_id, (path.stat().st_dev, path.stat().st_ino))
            self._check()
            return raw
        except (OSError, ValueError):
            self.budget.fail("download-path")

    def _current(self, path):
        self._need(isinstance(path, Path) and path in self._downloads, "owned-file")
        tag, raw, directory_id, file_id = self._downloads[path]
        try:
            self._need(_directory(path.parent) == directory_id and not path.is_symlink()
                       and not getattr(path, "is_junction", lambda: False)() and path.is_file()
                       and (path.stat().st_dev, path.stat().st_ino) == file_id, "owned-file")
            with path.open("rb") as stream:
                current = stream.read(records.MAX_MANIFEST_BYTES + 1)
            self._need(current == raw, "owned-file")
            return tag, raw
        except (OSError, ValueError):
            self.budget.fail("owned-file")

    def _verify(self, subject, flags, identity, *, code):
        self._need(self._scope == (subject, tuple(flags), identity) and self._verification_args is None, "verification-scope")
        self._verification_args = ["attestation", "verify", subject, *flags, "--format", "json"]
        try:
            return super()._verify(subject, flags, identity, code=code)
        except common.ReleaseError:
            self.budget.fail("signature")
        finally:
            self._verification_args = None

    def verify_file(self, path, *, signer_digest, source_digest):
        self._check()
        self._need(self._scope is None, "verification-scope")
        tag, raw = self._current(path)
        try:
            document = records.validate_manifest(common.loads_strict(raw))
        except common.ReleaseError:
            self.budget.fail("manifest")
        targets = {row.release: row.sha for row in self._snapshot.tags}
        self._need(raw == common.canonical_file_bytes(document) and document["release"] == tag
                   and document["sha"] == targets[tag]
                   and signer_digest == source_digest == document["signer"]["workflow_sha"], "verification-scope")
        flags = self.signer_flags(records.SHIP_SIGNER, signer_digest, source_digest, records.SLSA_PREDICATE)
        identity = self.signer_identity(records.SHIP_SIGNER, signer_digest, source_digest)
        self._scope = str(path), tuple(flags), identity
        try:
            result = super().verify_file(path, signer_digest=signer_digest, source_digest=source_digest)
            self._current(path)
            self._verified[tag] = raw
            self._check()
            return result
        finally:
            self._scope = None

    def verify_selected_images(self, record):
        self._check()
        self._need(self._image_scope is None and self._scope is None, "verification-scope")
        try:
            bootstrap = records.load_bootstrap()
        except common.ReleaseError:
            self.budget.fail("bootstrap")
        if record is None:
            document = bootstrap
        else:
            self._need(type(record) is records._AuthenticatedRecord and type(record.release) is str
                       and type(record.raw) is bytes and self._verified.get(record.release) == record.raw
                       and record.manifest_sha256 == common.sha256_bytes(record.raw), "image-binding")
            document = records.validate_manifest(common.loads_strict(record.raw))
            self._need(document["release"] == record.release and document["sha"] == record.sha, "image-binding")
        self._image_scope = document, bootstrap
        try:
            records.verify_images(self, document["images"], document["sha"], bootstrap)
            self._check()
            return VerifiedImages(document["release"], document["sha"],
                tuple((key, document["images"][key]) for key in common.COMPONENTS),
                tuple((key, bootstrap["legacy_signer"]["source_commit"]
                       if document["images"][key] == bootstrap["images"][key] else document["sha"])
                      for key in common.COMPONENTS))
        except common.ReleaseError:
            self.budget.fail("image-verification")
        finally:
            self._image_scope = None

    def verify_image(self, image, digest, *, signer_workflow, signer_digest, source_digest, predicate_type, bundle=None):
        self._check()
        self._need(self._image_scope is not None and self._scope is None and bundle is None, "image-scope")
        document, bootstrap = self._image_scope
        components = [key for key in common.COMPONENTS if common.IMAGE_REPOSITORY[key] == image
                      and document["images"][key] == digest]
        self._need(len(components) == 1, "image-scope")
        key = components[0]
        if digest == bootstrap["images"][key]:
            legacy = bootstrap["legacy_signer"]
            expected = legacy["workflow"], legacy["workflow_sha"], legacy["workflow_sha"]
            predicates = (records.SLSA_PREDICATE, legacy["source_predicate"])
        else:
            expected = records.SHIP_SIGNER, document["sha"], document["sha"]
            predicates = (records.SLSA_PREDICATE,)
        self._need((signer_workflow, signer_digest, source_digest) == expected and predicate_type in predicates, "image-scope")
        flags = self.signer_flags(signer_workflow, signer_digest, source_digest, predicate_type)
        identity = self.signer_identity(signer_workflow, signer_digest, source_digest)
        self._scope = f"oci://{image}@{digest}", tuple(flags), identity
        try:
            return super().verify_image(image, digest, signer_workflow=signer_workflow,
                signer_digest=signer_digest, source_digest=source_digest, predicate_type=predicate_type)
        finally:
            self._scope = None

    def recheck(self, snapshot, *, selected_tag, latest_tag):
        self._check()
        self._need(snapshot is self._snapshot and type(snapshot) is InventorySnapshot, "snapshot")
        tags = {row.release: row for row in snapshot.tags}
        releases = {row.release: row for row in snapshot.records}
        self._need(all(type(tag) is str and (tag == common.BOOTSTRAP_TAG or tag in releases)
                       for tag in (selected_tag, latest_tag)), "selection")
        self._need(latest_tag == (snapshot.records[-1].release if snapshot.records else common.BOOTSTRAP_TAG), "selection")
        current, _ = self._read_snapshot()
        self._need(current == snapshot, "changed")
        for tag in sorted({selected_tag, latest_tag} - {common.BOOTSTRAP_TAG}):
            path = API + "/releases/tags/" + tag
            self._tag_gets.add(path)
            self._need(self._release(self._get(path)) == releases[tag] and self._exact_ref(tag) == tags[tag], "changed")
        self._check()
        self._finished = True
        return snapshot

    def run(self, args, *, code):
        self.budget.fail("write")
