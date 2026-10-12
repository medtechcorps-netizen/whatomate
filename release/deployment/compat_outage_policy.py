"""Fixed consumer-only data loading; absent configuration grants no coverage.

The data file is intentionally absent. Publisher paths never call this loader;
its code may be in a reviewed publisher closure, but the separate data is not.
"""
import os
from pathlib import Path
import stat

import compat_negative_index as policy

DATA_PATH = "release/deployment/compat-negative-consumer-v1.json"
_POLICY_PATH = Path(__file__).absolute().with_name("compat-negative-consumer-v1.json")


def _need(value):
    if not value:
        raise policy.NegativeIndexRefusal()


def _identity(value):
    return (value.st_dev, value.st_ino, value.st_mode, value.st_size,
            value.st_mtime_ns, value.st_ctime_ns, value.st_nlink,
            getattr(value, "st_birthtime_ns", None))


def _cross(left, right):
    slot = 7 if left[7] is not None and right[7] is not None else 5
    return left[:5] == right[:5] and left[6] == right[6] and left[slot] == right[slot]


def _parents(path):
    rows = []
    for parent in path.parents:
        _need(not parent.is_symlink() and not getattr(parent, "is_junction", lambda: False)())
        value = parent.lstat()
        _need(stat.S_ISDIR(value.st_mode))
        rows.append((value.st_dev, value.st_ino, value.st_mode))
    return tuple(rows)


def parse_consumer_policy(raw):
    """Same fixed grammar, with the consumer data excluded from every closure."""
    parsed = policy.parse_consumer_policy(raw)
    _need(all(row.path != DATA_PATH for lineage in parsed.lineage for row in lineage.inventory))
    return parsed


def load_consumer_policy():
    """No caller path/env override, import-time read or placeholder producer."""
    try:
        path = _POLICY_PATH
        _need(type(path) is type(Path()) and path.is_absolute())
        parents = _parents(path)
        try:
            before = path.lstat()
        except FileNotFoundError:
            _need(_parents(path) == parents)
            return None
        _need(not path.is_symlink() and not getattr(path, "is_junction", lambda: False)()
              and stat.S_ISREG(before.st_mode) and before.st_nlink == 1
              and 0 < before.st_size <= policy.MAX_CONFIG)
        before = _identity(before)
        with path.open("rb") as stream:
            opened = _identity(os.fstat(stream.fileno()))
            _need(_cross(before, opened))
            raw = stream.read(policy.MAX_CONFIG + 1)
            _need(_identity(os.fstat(stream.fileno())) == opened)
        _need(len(raw) == before[3] and 0 < len(raw) <= policy.MAX_CONFIG
              and _identity(path.lstat()) == before and _parents(path) == parents)
        return parse_consumer_policy(raw)
    except policy.NegativeIndexRefusal:
        raise
    except (OSError, ValueError, TypeError, AttributeError, OverflowError, RecursionError):
        raise policy.NegativeIndexRefusal() from None


def require_consumer_policy():
    value = load_consumer_policy()
    if value is None:
        raise policy.NegativeIndexRefusal("negative-index-unconfigured")
    return value
