"""Fixed negative-intent reads/publication under a caller-owned allowance.

This uncalled preparatory transport installs no setup or coverage policy. Raw
observations and publication receipts do not grant compatibility or outage
authority. Only the owning operation closes the shared OperationBudget.
"""
from contextlib import contextmanager
import hashlib
from pathlib import Path
import re
import stat

import compat_negative_index as policy

HEADERS = ("-H", "Accept: application/vnd.github+json",
           "-H", "X-GitHub-Api-Version: 2022-11-28")
MAX_COMMANDS = 10
MAX_BYTES = 2 << 20
MAX_BINARY = 256 << 20
BODY_LIMIT = 64 << 10
HTTP_LIMIT = (16 + 64) << 10
STDERR_LIMIT = 64 << 10
_SHA = re.compile(r"[0-9a-f]{40}\Z")


def _need(value):
    if not value:
        raise policy.NegativeIndexRefusal()


def open_reader(binary, token, config_dir, *, budget, base_env=None):
    import compat_outage_policy
    installed = compat_outage_policy.require_consumer_policy()  # Before client effects.
    _need(type(installed) is policy.ConsumerPolicy)
    replayed = policy.parse_consumer_policy(installed.raw)
    _need(policy._same(replayed, installed))
    return _open(NegativeReader, replayed.setup, binary, token, config_dir,
                 budget=budget, base_env=base_env)


def open_publisher(binary, token, config_dir, *, budget, base_env=None):
    installed = policy.load_producer_setup()  # Before paths, env or imports.
    return _open(NegativePublisher, installed, binary, token, config_dir,
                 budget=budget, base_env=base_env)


def _open(kind, setup, binary, token, config_dir, *, budget, base_env=None):
    """Private concrete seam; explicit synthetic data is never installed trust."""
    import record_transport as transport
    import ship_common as common

    _need(kind in (NegativeReader, NegativePublisher)
          and type(budget) is transport.OperationBudget)
    client = object.__new__(kind)
    client._budget, client._transport, client._common = budget, transport, common
    client._calls = client._bytes = 0
    client._rule = None
    client._refs = {}
    client._publication_started = False
    try:
        budget.check()
        policy._valid_setup(setup)
        client._setup = setup
        _need(type(token) is str and 0 < len(token) <= 4096
              and all(33 <= ord(char) <= 126 for char in token))
        _need(isinstance(binary, Path) and binary.is_absolute()
              and isinstance(config_dir, Path) and config_dir.is_absolute())
        client._binary, client._config = binary, config_dir
        client._config_identity = transport._directory(config_dir, empty=True)
        client._binary_identity = client._binary_pin()
        client._env = common.scrubbed_env({
            "GH_TOKEN": token, "GH_CONFIG_DIR": str(config_dir),
            "GH_HOST": "github.com", "GH_PROMPT_DISABLED": "1",
            "GH_NO_UPDATE_NOTIFIER": "1", "GH_NO_EXTENSION_UPDATE_NOTIFIER": "1",
            "NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0",
        }, base=base_env)
        with client._guard():
            result = client._capture(["--version"], limit=4096, timeout=5)
            _need(result.returncode == 0 and not result.stderr)
            lines = result.stdout.decode("ascii").splitlines()
            _need(lines and re.fullmatch(r"gh version 2\.98\.0(?: \([ -~]{1,100}\))?", lines[0]))
            raw = client._get(policy.API)
            value = policy._load(raw, BODY_LIMIT)
            _need(type(value) is dict and type(value.get("id")) is int
                  and value["id"] == policy.REPOSITORY_ID
                  and value.get("full_name") == policy.REPOSITORY)
        return client
    except Exception:
        client._refuse()


class _Client:
    def __init__(self, *args, **kwargs):
        # Public construction cannot install caller-selected setup or capture.
        raise policy.NegativeIndexRefusal()

    @property
    def budget(self):
        return self._budget

    @property
    def setup(self):
        return self._setup

    def _refuse(self):
        try:
            self._budget.fail("negative-index")
        except self._transport.TransportRefusal:
            raise policy.NegativeIndexRefusal() from None

    @contextmanager
    def _guard(self):
        try:
            self._check()
            yield
            self._check()
        except Exception:
            self._refuse()

    def _binary_pin(self):
        for parent in self._binary.parents:
            _need(parent.is_dir() and not parent.is_symlink()
                  and not getattr(parent, "is_junction", lambda: False)())
        before = self._binary.lstat()
        _need(stat.S_ISREG(before.st_mode) and not self._binary.is_symlink()
              and not getattr(self._binary, "is_junction", lambda: False)()
              and 0 < before.st_size <= MAX_BINARY)
        digest = hashlib.sha256()
        total = 0
        with self._binary.open("rb") as stream:
            while chunk := stream.read(1 << 20):
                self._budget.check()
                total += len(chunk)
                _need(total <= MAX_BINARY)
                digest.update(chunk)
        after = self._binary.lstat()
        fields = ("st_dev", "st_ino", "st_mode", "st_size", "st_mtime_ns", "st_ctime_ns")
        before_id = tuple(getattr(before, key) for key in fields)
        _need(before_id == tuple(getattr(after, key) for key in fields)
              and total == before.st_size)
        return before_id, digest.hexdigest()

    def _check(self):
        self._budget.check()
        _need(self._transport._directory(self._config, empty=True) == self._config_identity
              and self._binary_pin() == self._binary_identity)
        policy._valid_setup(self._setup)
        self._budget.check()

    def _capture(self, args, *, limit=BODY_LIMIT, timeout=30):
        self._check()
        allowance = MAX_BYTES - self._bytes
        _need(self._calls < MAX_COMMANDS and allowance > STDERR_LIMIT)
        self._calls += 1
        result = self._budget.capture([str(self._binary), *args], env=dict(self._env),
            timeout=timeout, stdout_limit=min(limit, allowance - STDERR_LIMIT),
            stderr_limit=STDERR_LIMIT)
        self._bytes += len(result.stdout) + len(result.stderr)
        self._check()
        return result

    @staticmethod
    def _args(method, path, *, include=False):
        args = ["api", "--hostname", "github.com", "--method", method, path]
        if include:
            args.append("--include")
        return [*args, *HEADERS]

    def _get(self, path):
        result = self._capture(self._args("GET", path))
        _need(result.returncode == 0 and not result.stderr)
        return result.stdout

    def read_rule(self):
        with self._guard():
            raw = self._get(policy.API + "/rulesets/" + str(self._setup.rule_id))
            policy.replay_rule(raw, self._setup)
            _need(self._rule is None or self._rule == raw)
            self._rule = raw
        return raw

    def read_refs(self, producer_sha):
        with self._guard():
            _need(type(producer_sha) is str and _SHA.fullmatch(producer_sha))
            raw = self._get(policy.API + "/git/matching-refs/tags/" + policy.PREFIX + producer_sha + "-")
            policy.replay_refs(raw, producer_sha=producer_sha)
            _need(producer_sha not in self._refs or self._refs[producer_sha] == raw)
            self._refs[producer_sha] = raw
        return raw

    def observe_actions_service(self):
        with self._guard():
            result = self._capture(self._args("GET", policy.ACTIONS_ENDPOINT, include=True), limit=HTTP_LIMIT)
            observed = policy.replay_http(result.stdout, endpoint=policy.ACTIONS_ENDPOINT)
            _need((observed.status == 200 and result.returncode == 0 and not result.stderr)
                  or (observed.status in (502, 503, 504) and result.returncode == 1))
        return result.stdout


class NegativeReader(_Client):
    """Fixed raw reader; observed emptiness and unavailability are not coverage."""


class NegativePublisher(_Client):
    """One exact create/readback attempt; no update, delete, force or retry."""

    @staticmethod
    def _ref_body(body, ref, producer_sha):
        value = policy._load(body, BODY_LIMIT)
        _need(type(value) is dict and set(value) == {"ref", "node_id", "url", "object"})
        _need(value["ref"] == ref and type(value["node_id"]) is str
              and 0 < len(value["node_id"]) <= 256
              and all(33 <= ord(char) <= 126 for char in value["node_id"]))
        _need(value["url"] == "https://api.github.com" + policy.API + "/git/" + ref)
        obj = value["object"]
        _need(type(obj) is dict and set(obj) == {"type", "sha", "url"}
              and obj["type"] == "commit" and obj["sha"] == producer_sha
              and obj["url"] == "https://api.github.com" + policy.API + "/git/commits/" + producer_sha)
        return value

    def _exact_ref(self, ref, producer_sha, *, allow_absent):
        path = policy.API + "/git/ref/" + ref.removeprefix("refs/")
        result = self._capture(self._args("GET", path, include=True), limit=HTTP_LIMIT)
        status, body, headers = policy._http_frame(result.stdout)
        _need(headers.get(b"content-type", b"").lower() in
              (b"application/json", b"application/json; charset=utf-8"))
        if status == 404 and allow_absent:
            value = policy._load(body, BODY_LIMIT)
            _need(result.returncode == 1 and type(value) is dict
                  and set(value) <= {"message", "documentation_url", "status"}
                  and value.get("message") == "Not Found"
                  and ("status" not in value or value["status"] == "404")
                  and ("documentation_url" not in value or value["documentation_url"] ==
                       "https://docs.github.com/rest/git/refs#get-a-reference"))
            return None
        _need(status == 200 and result.returncode == 0 and not result.stderr)
        return self._ref_body(body, ref, producer_sha)

    def publish_intent(self, request):
        with self._guard():
            _need(not self._publication_started)
            policy.validate_publication_request(request)
            ref = policy.marker_ref(request)
            # Sticky even if the first read fails; a caller cannot retry an
            # ambiguous create by invoking this method again on the client.
            self._publication_started = True
            self.read_rule()
            existing = self._exact_ref(ref, request.producer_sha, allow_absent=True)
            if existing is None:
                args = self._args("POST", policy.API + "/git/refs", include=True)
                args.extend(("--raw-field", "ref=" + ref, "--raw-field", "sha=" + request.producer_sha))
                result = self._capture(args, limit=HTTP_LIMIT)
                status, body, headers = policy._http_frame(result.stdout,
                    expected_created_location="https://api.github.com" + policy.API + "/git/" + ref)
                _need(status == 201 and result.returncode == 0 and not result.stderr
                      and headers.get(b"content-type", b"").lower() in
                      (b"application/json", b"application/json; charset=utf-8"))
                existing = self._ref_body(body, ref, request.producer_sha)
            confirmed = self._exact_ref(ref, request.producer_sha, allow_absent=False)
            _need(existing == confirmed)
            self.read_rule()
            receipt = policy.IntentPersisted(request, self._setup.sha256, ref)
        return receipt
