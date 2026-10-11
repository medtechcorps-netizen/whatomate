"""Pure replay of complete pending source bytes; no source or SQL authority.

The concrete resolver must enumerate both immutable directories, retain every
regular blob and authenticate their candidate/latest-record revisions. Empty
tuples then mean observed absence; missing historical material is not absence.
Active catalog bytes remain separate and are never replaced by a pending target.
The parsed object graph is ephemeral; only raw material and the small binding
belong in protected custody and signed compatibility context respectively.
Current callers replay these bytes before accepting the derived binding.
"""
from dataclasses import dataclass
import re

import pending_plan as plan

MAX_FILE_BYTES = 4 * 1024 * 1024
MAX_MATERIAL_BYTES = 8 * 1024 * 1024
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_FILE = re.compile(re.escape(plan.PREFIX) + r"(?:catalog-v[1-9][0-9]{0,9}\.json|global_tables\.pending\.json|history\.pending\.json)\Z")


class PendingSourceRefusal(ValueError):
    """Fixed errors without file content, paths or other input text."""


@dataclass(frozen=True, slots=True)
class PendingBlob:
    path: str
    raw: bytes


@dataclass(frozen=True, slots=True)
class PendingMaterial:
    candidate_sha: str
    previous_sha: str
    candidate_files: tuple[PendingBlob, ...]
    previous_files: tuple[PendingBlob, ...]


@dataclass(frozen=True, slots=True)
class PendingBinding:
    format_version: int
    candidate_sha: str
    previous_sha: str
    candidate_digest: str
    previous_digest: str
    active_artifacts: tuple[tuple[str, str], ...]
    candidate_files: tuple[tuple[str, str, int], ...]
    previous_files: tuple[tuple[str, str, int], ...]
    target_version: int | None


@dataclass(frozen=True, slots=True)
class PendingProjection:
    binding: PendingBinding
    plan: plan.PendingPlan | None


def _need(value, code="material"):
    if not value:
        raise PendingSourceRefusal("pending-source-invalid:" + code)


def _sha(value):
    return type(value) is str and _SHA.fullmatch(value) is not None


def _files(rows):
    _need(type(rows) is tuple and len(rows) in (0, 3))
    result, inventory = {}, []
    previous = ""
    for row in rows:
        _need(type(row) is PendingBlob and type(row.path) is str
              and _FILE.fullmatch(row.path) is not None and row.path > previous
              and type(row.raw) is bytes and 0 < len(row.raw) <= MAX_FILE_BYTES)
        previous = row.path
        result[row.path] = row.raw
        inventory.append((row.path, plan.digest(row.raw), len(row.raw)))
    return result, tuple(inventory)


def replay(material, *, candidate_sha, previous_sha, active_catalog,
           active_globals, active_history) -> PendingProjection:
    """Reparse exact bytes and continuity; never trust an already-parsed plan.

previous_sha means the latest release record's source, not the selected image
or retained database origin. The caller must supply those exact identities and
the candidate's separately authenticated active bytes. First-parent transient
history checks are still the classifier/resolver's independent obligation.
"""
    _need(_sha(candidate_sha) and _sha(previous_sha), "source")
    _need(type(material) is PendingMaterial and _sha(material.candidate_sha)
          and _sha(material.previous_sha) and material.candidate_sha == candidate_sha
          and material.previous_sha == previous_sha, "source")
    active = (("active_catalog", active_catalog), ("active_globals", active_globals),
              ("active_history", active_history))
    _need(all(type(raw) is bytes and 0 < len(raw) <= MAX_FILE_BYTES for _, raw in active), "bound")
    current, current_inventory = _files(material.candidate_files)
    previous, previous_inventory = _files(material.previous_files)
    # Repeated occurrences are deliberately charged, not just unique digests.
    _need(sum(len(raw) for _, raw in active) + sum(map(len, current.values()))
          + sum(map(len, previous.values())) <= MAX_MATERIAL_BYTES, "bound")
    _need(candidate_sha != previous_sha or material.candidate_files == material.previous_files, "same-source")
    _need(current or not previous, "removed")
    parsed = None
    if current:
        try:
            parsed = plan.parse_bundle(active_catalog=active_catalog, active_globals=active_globals,
                active_history=active_history, files=current, prior_files=previous if previous else None)
        except plan.PendingRefusal:
            raise PendingSourceRefusal("pending-source-invalid:bundle") from None
    binding = PendingBinding(1, candidate_sha, previous_sha,
        plan.digest(plan.canonical(current_inventory)), plan.digest(plan.canonical(previous_inventory)),
        tuple((name, plan.digest(raw)) for name, raw in active), current_inventory, previous_inventory,
        parsed.entries[0].version if parsed is not None else None)
    return PendingProjection(binding, parsed)
