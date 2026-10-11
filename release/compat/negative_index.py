"""Disabled publisher for complete negative compatibility measurements.

The installed setup stays absent in the preparatory leaf. A later reviewed
caller must run this before signing the exact public subjects. Local parsing
and an intent receipt are not signatures, source eligibility or outage cover.
"""
from dataclasses import dataclass
import hashlib
import os
from pathlib import Path
import stat
import sys
import time

# Lexical import setup only: do not resolve paths, inspect the environment or
# import the operational CLI/transport before checking the absent configuration.
sys.path.insert(0, str(Path(__file__).parent.parent / "deployment"))
import compat_negative_index as policy


_PUBLIC = 16384
_CONTEXT = 4096
_INPUT_NAMES = frozenset({"resolution.json", "compat-result.json", "compat-context.json"})
_SUBJECT_NAMES = frozenset({"compat-result.json", "compat-context.json"})
_REFUSED = "negative-index-publisher-refused"


def _need(value):
    if not value:
        raise policy.NegativeIndexRefusal()


def _operational_modules():
    # Existing modules perform their own import-path setup. They are deliberately
    # unreachable from an unconfigured operational entry, including bad argv.
    import cli
    import compat_negative_transport as transport
    return cli, transport, cli.record_transport


def _file_identity(value):
    return (value.st_dev, value.st_ino, value.st_mode, value.st_size,
            value.st_mtime_ns, value.st_ctime_ns, value.st_nlink,
            getattr(value, "st_birthtime_ns", None))


def _same_file_identity(path_identity, handle_identity):
    # Windows stat can still expose birthtime as ctime while fstat exposes the
    # actual change time. Each API's complete identity is checked separately.
    time_slot = 7 if path_identity[7] is not None and handle_identity[7] is not None else 5
    return (path_identity[:5] == handle_identity[:5]
            and path_identity[6] == handle_identity[6]
            and path_identity[time_slot] == handle_identity[time_slot])


def _identity(path, cli, *, directory=False):
    cli.regular_path(path, directory=directory)
    value = path.stat()
    _need(stat.S_ISDIR(value.st_mode) if directory else stat.S_ISREG(value.st_mode))
    _need(directory or value.st_nlink == 1)
    identity = (value.st_dev, value.st_ino, value.st_mode, value.st_size,
                value.st_mtime_ns, value.st_ctime_ns)
    return identity if directory else _file_identity(value)


def _directory(path, names, cli):
    before = _identity(path, cli, directory=True)
    found = set()
    with os.scandir(path) as entries:
        for entry in entries:
            found.add(entry.name)
            _need(len(found) <= len(names))
    _need(found == names)
    children = tuple((name, _identity(path / name, cli)) for name in sorted(names))
    _need(_identity(path, cli, directory=True) == before)
    return before, children


def _read(path, limit, cli, budget):
    budget.check()
    before = _identity(path, cli)
    with path.open("rb") as stream:
        opened = _file_identity(os.fstat(stream.fileno()))
        _need(_same_file_identity(before, opened))
        raw = stream.read(limit + 1)
        _need(_file_identity(os.fstat(stream.fileno())) == opened)
    _need(type(raw) is bytes and 0 < len(raw) <= limit)
    _need(_identity(path, cli) == before)
    budget.check()
    return raw


@dataclass(frozen=True, slots=True)
class _Subjects:
    snapshot: bytes
    result: bytes
    context: bytes
    root_identity: tuple
    input_inventory: tuple
    work_identity: tuple
    public_inventory: tuple


def _subjects(root, ctx, cli, budget):
    """Read the actual attested pair, never the two downloaded evidence copies."""
    budget.check()
    root_identity = _identity(root, cli, directory=True)
    inputs = root / "compat-input"
    work = root / ("compat-validate-evidence-" + ctx.suffix)
    public = work / "public"
    input_inventory = _directory(inputs, _INPUT_NAMES, cli)
    work_identity = _identity(work, cli, directory=True)
    public_inventory = _directory(public, _SUBJECT_NAMES, cli)
    snapshot = _read(inputs / "resolution.json", _PUBLIC, cli, budget)
    result = _read(public / "compat-result.json", _PUBLIC, cli, budget)
    context = _read(public / "compat-context.json", _CONTEXT, cli, budget)
    _need(_directory(inputs, _INPUT_NAMES, cli) == input_inventory
          and _directory(public, _SUBJECT_NAMES, cli) == public_inventory
          and _identity(work, cli, directory=True) == work_identity)
    # Creating our own private scratch changes RUNNER_TEMP's directory times.
    # Preserve its device/inode/mode, not mutable directory-size/time counters.
    _need(_identity(root, cli, directory=True)[:3] == root_identity[:3])
    budget.check()
    return _Subjects(snapshot, result, context, root_identity[:3], input_inventory,
                     work_identity, public_inventory)


def _validate(value, ctx, cli):
    _need(type(value) is _Subjects)
    snapshot = cli.common.loads_strict(value.snapshot)
    if ctx.target is None:
        _need(cli.canonical(cli.validate_snapshot(snapshot, ctx)) == value.snapshot)
        measured = cli.validate_result(value.result, snapshot, ctx)
        cli.validate_evidence(value.context, value.result, snapshot, ctx)
        protocol = "normal3"
    else:
        cli._rollback_context(ctx)
        _need(cli.canonical(cli.validate_rollback_snapshot(snapshot, ctx)) == value.snapshot)
        measured = cli.validate_rollback_result(value.result, snapshot, ctx)
        cli.validate_rollback_evidence(value.context, value.result, snapshot, ctx)
        protocol = "rollback4"
    request = policy.PublicationRequest(ctx.sha, ctx.run_id, ctx.attempt,
        hashlib.sha256(value.result).hexdigest(), hashlib.sha256(value.context).hexdigest(), protocol)
    policy.validate_publication_request(request)
    return request, cli.accepted(measured)


def _reread(original, root, ctx, cli, budget, request, positive):
    current = _subjects(root, ctx, cli, budget)
    _need(current == original)
    _need(_validate(current, ctx, cli) == (request, positive))
    budget.check()


def _config_directory(root, ctx, cli, budget):
    budget.check()
    work = root / ("compat-negative-index-" + ctx.suffix)
    _need(not work.exists() and not work.is_symlink()
          and not getattr(work, "is_junction", lambda: False)())
    work.mkdir(mode=0o700)
    cli.regular_path(work, directory=True)
    config = work / "gh-config"
    config.mkdir(mode=0o700)
    cli.regular_path(config, directory=True)
    budget.check()
    return config


def publish_from_environment(*, budget):
    """Fixed local subjects and a concrete transport under one original budget.

    Caller-created data is not authority. There is no raw input, verdict, client,
    policy, directory or capture override at this operational boundary.
    """
    setup = policy.load_producer_setup()  # FIRST: None has no operational effects.
    cli, transport, source = _operational_modules()
    _need(type(budget) is source.OperationBudget)
    try:
        budget.check()
        _need(type(setup) is policy.ProducerSetup
              and policy.parse_producer_setup(setup.raw) == setup)
        ctx = cli.context(os.environ)
        _need(ctx.ref == "refs/heads/main" and ctx.event in {"push", "workflow_dispatch"})
        if ctx.target is not None:
            cli._rollback_context(ctx)
        _need(os.environ.get("COMPAT_PREVIEW_OUTCOME") == "success"
              and os.environ.get("COMPAT_RUN_OUTCOME") == "success")
        root = cli.regular_path(Path(os.environ.get("RUNNER_TEMP", "")), directory=True)
        original = _subjects(root, ctx, cli, budget)
        request, positive = _validate(original, ctx, cli)
        if positive:
            _reread(original, root, ctx, cli, budget, request, True)
            return policy.NoIntentRequired(request, setup.sha256)
        # Credentials, executables, scratch and clients are negative-only.
        token = os.environ.get("GH_TOKEN")
        _need(type(token) is str and 0 < len(token) <= 8192
              and not any(character in token for character in "\r\n\0"))
        binary = cli.regular_path(Path(os.environ.get("COMPAT_GH_BIN", "")))
        config = _config_directory(root, ctx, cli, budget)
        client = transport.open_publisher(binary, token, config, budget=budget,
                                          base_env=cli.common.scrubbed_env(base=os.environ))
        _need(type(client) is transport.NegativePublisher and client.budget is budget
              and client.setup == setup)
        receipt = client.publish_intent(request)
        _need(type(receipt) is policy.IntentPersisted)
        policy.validate_publication_request(receipt.request)
        _need(receipt.request == request and type(receipt.setup_sha256) is str
              and type(receipt.marker_ref) is str
              and receipt.setup_sha256 == setup.sha256
              and receipt.marker_ref == policy.marker_ref(request))
        _reread(original, root, ctx, cli, budget, request, False)
        return receipt
    except BaseException as error:
        # Local parsing/file failures poison this same allowance too. Never give
        # a caller a fresh allowance or reinterpret a partial mutation as empty.
        budget.failed = True
        if not isinstance(error, Exception):
            raise
        raise policy.NegativeIndexRefusal() from None


def main(argv=None):
    budget = None
    try:
        policy.load_producer_setup()  # Before argv, env, operational imports or clock.
        args = sys.argv[1:] if argv is None else argv
        _need(type(args) is list and not args)
        _, _, source = _operational_modules()
        budget = source.OperationBudget(time.monotonic() + 600)
        publish_from_environment(budget=budget)
        budget.close()
        return 0
    except Exception as error:
        # The sole distinguished public code is module-owned absence. Avoid
        # formatting raw exceptions, streams, paths, tokens or documents.
        absent = (type(error) is policy.NegativeIndexRefusal
                  and error.args == ("negative-index-unconfigured",))
        print("negative-index-unconfigured" if absent else _REFUSED, file=sys.stderr)
        return 1
    finally:
        if budget is not None:
            # The success path already closed before returning zero. This is
            # only failure/cancellation cleanup and must not expose raw errors.
            try:
                budget.close()
            except Exception:
                pass


if __name__ == "__main__":
    raise SystemExit(main())
