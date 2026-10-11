"""Closed pending-inventory wire data, without source or execution authority.

The outer context decoder must reject duplicate JSON fields and noncanonical
bytes before calling parse_wire. This module accepts only the decoded, exact
JSON shape. Hashes and declared lengths do not prove complete source capture,
active+1, object coverage or a valid plan: pending_source.replay must still
validate the raw authenticated material. No current producer or gate uses this
preparatory format. Historical null is deliberately not observed absence.
"""
import re

import pending_source as source

_KEYS = frozenset({"format_version", "candidate_sha", "previous_sha",
    "candidate_digest", "previous_digest", "active_artifacts",
    "candidate_files", "previous_files", "target_version"})
_ACTIVE = ("active_catalog", "active_globals", "active_history")
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_CATALOG = re.compile(re.escape(source.plan.PREFIX) + r"catalog-v([1-9][0-9]{0,9})\.json\Z")


class PendingBindingRefusal(ValueError):
    """Fixed diagnostic codes, never input paths, hashes or private text."""


def _need(ok, code):
    if not ok:
        raise PendingBindingRefusal("pending-binding-invalid:" + code)


def _sha(value):
    return type(value) is str and len(value) == 40 and _SHA.fullmatch(value) is not None


def _hash(value):
    return type(value) is str and len(value) == 64 and _HASH.fullmatch(value) is not None


def _active(rows, container):
    _need(type(rows) is container and len(rows) == 3, "active")
    out = []
    for name, row in zip(_ACTIVE, rows):
        _need(type(row) is container and len(row) == 2
              and type(row[0]) is str and row[0] == name and _hash(row[1]), "active")
        out.append((name, row[1]))
    return tuple(out)


def _inventory(rows, container):
    _need(type(rows) is container and len(rows) in (0, 3), "inventory")
    out, catalogs, previous = [], [], ""
    for row in rows:
        _need(type(row) is container and len(row) == 3, "inventory")
        path, digest, length = row
        _need(type(path) is str and len(path) <= 128 and path > previous
              and _hash(digest) and type(length) is int
              and 0 < length <= source.MAX_FILE_BYTES, "inventory")
        match = _CATALOG.fullmatch(path)
        _need(match is not None or path in (source.plan.GLOBALS, source.plan.PLAN), "inventory")
        if match:
            version = int(match[1])
            _need(1 <= version <= 2**31 - 1, "target")
            catalogs.append(version)
        out.append((path, digest, length))
        previous = path
    _need(not out or (len(catalogs) == 1
          and {row[0] for row in out} == {out[0][0], source.plan.GLOBALS, source.plan.PLAN}), "inventory")
    return tuple(out), catalogs[0] if catalogs else None


def _checked(binding):
    _need(type(binding) is source.PendingBinding, "binding")
    _need(type(binding.format_version) is int and binding.format_version == 1, "version")
    _need(_sha(binding.candidate_sha) and _sha(binding.previous_sha), "source")
    _active(binding.active_artifacts, tuple)
    current, target = _inventory(binding.candidate_files, tuple)
    previous, _ = _inventory(binding.previous_files, tuple)
    _need(sum(row[2] for row in current + previous) <= source.MAX_MATERIAL_BYTES, "bound")
    _need(_hash(binding.candidate_digest) and _hash(binding.previous_digest)
          and binding.candidate_digest == source.plan.digest(source.plan.canonical(current))
          and binding.previous_digest == source.plan.digest(source.plan.canonical(previous)), "digest")
    _need((binding.target_version is None if target is None else
           type(binding.target_version) is int and binding.target_version == target), "target")
    _need(not previous or current == previous, "continuity")
    _need(binding.candidate_sha != binding.previous_sha or current == previous, "same-source")
    return binding


def parse_wire(value: object) -> source.PendingBinding:
    """Parse closed JSON primitives; no historical-null or missing-field fallback."""
    _need(type(value) is dict and len(value) == len(_KEYS)
          and all(type(key) is str for key in value) and value.keys() == _KEYS, "fields")
    active = _active(value["active_artifacts"], list)
    current, _ = _inventory(value["candidate_files"], list)
    previous, _ = _inventory(value["previous_files"], list)
    return _checked(source.PendingBinding(value["format_version"], value["candidate_sha"],
        value["previous_sha"], value["candidate_digest"], value["previous_digest"],
        active, current, previous, value["target_version"]))


def to_wire(binding: source.PendingBinding) -> dict:
    """Return detached JSON data only after checking every immutable field."""
    value = _checked(binding)
    return {"format_version": value.format_version, "candidate_sha": value.candidate_sha,
        "previous_sha": value.previous_sha, "candidate_digest": value.candidate_digest,
        "previous_digest": value.previous_digest,
        "active_artifacts": [list(row) for row in value.active_artifacts],
        "candidate_files": [list(row) for row in value.candidate_files],
        "previous_files": [list(row) for row in value.previous_files],
        "target_version": value.target_version}


def validate_against(binding: source.PendingBinding, *, candidate_sha: str,
                     previous_sha: str, active_artifacts: tuple) -> source.PendingBinding:
    """Compare caller-supplied source roles; these data are not auth capabilities.

    previous_sha is the latest record's source, not its retained database origin
    or its actual selected web-image source. Active hashes must come from the
    candidate's separate authenticated active bytes. Raw replay additionally
    charges those active bytes against the aggregate material cap.
    """
    value = _checked(binding)
    _need(_sha(candidate_sha) and _sha(previous_sha)
          and value.candidate_sha == candidate_sha and value.previous_sha == previous_sha, "source")
    _need(value.active_artifacts == _active(active_artifacts, tuple), "active")
    return value
