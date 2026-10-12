"""Closed negative-intent data replay; no source, transport or outage authority.

Producer setup records a reviewed configuration observation only; it does not
prove hosted permissions or outage authority. Consumer policy and data remain
absent. Future consumer policy bytes belong outside the publisher's import/read/
build closure, not in this module's own source inventory. Parsed holders and
receipts are ordinary data.
"""
from dataclasses import dataclass, fields, is_dataclass
import datetime as dt
import hashlib
import json
import re

REPOSITORY = "medtechcorps-netizen/whatomate"
REPOSITORY_ID = 1297344686
PREFIX = "rereply-compat-negative-v1-"
API = "/repos/" + REPOSITORY
ACTIONS_ENDPOINT = API + "/actions/workflows/compat.yml/runs?branch=main&event=push&per_page=1&page=1"
MAX_CONFIG = 128 << 10
MAX_WITNESS = MAX_BODY = 64 << 10
MAX_HEADERS = 16 << 10
MAX_REFS = 256
MAX_SOURCE = 4 << 20
MAX_SOURCE_TOTAL = 8 << 20
MAX_INVENTORY = 128
MAX_LINEAGE = 16
MAX_DEPTH = 16
MAX_NODES = 8192
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_EPOCH = re.compile(r"[a-z][a-z0-9-]{0,63}\Z")
_TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?(?:Z|[+-][0-9]{2}:[0-9]{2})\Z")
_INSTALLED_PRODUCER_SETUP: bytes | None = (
    b'{"epoch":"rereply-compat-negative-v1","format_version":1,"prefix":"rereply-compat-negative-v1-",'
    b'"repository":"medtechcorps-netizen/whatomate","repository_id":1297344686,"setup_review_sha256":"'
    b'7c656bc617731209f4ee32e8d0f6fff919fcb3f898d2812e19dd07ea529def3e","setup_witness":"{\\"complete_o'
    b'riginal_post_response\\":{\\"_links\\":{\\"html\\":{\\"href\\":\\"https://github.com/medtechcorps-netize'
    b'n/whatomate/rules/24896373\\"},\\"self\\":{\\"href\\":\\"https://api.github.com/repos/medtechcorps-net'
    b'izen/whatomate/rulesets/24896373\\"}},\\"bypass_actors\\":[],\\"conditions\\":{\\"ref_name\\":{\\"exclud'
    b'e\\":[],\\"include\\":[\\"refs/tags/rereply-compat-negative-v1-*\\"]}},\\"created_at\\":\\"2026-10-12T06'
    b':08:39.569+08:00\\",\\"current_user_can_bypass\\":\\"never\\",\\"enforcement\\":\\"active\\",\\"id\\":24896'
    b'373,\\"name\\":\\"rereply-compat-negative-v1\\",\\"node_id\\":\\"RRS_lACqUmVwb3NpdG9yec5NU-iuzgF743U\\",'
    b'\\"rules\\":[{\\"parameters\\":{\\"update_allows_fetch_and_merge\\":false},\\"type\\":\\"update\\"},{\\"typ'
    b'e\\":\\"deletion\\"}],\\"source\\":\\"medtechcorps-netizen/whatomate\\",\\"source_type\\":\\"Repository\\",'
    b'\\"target\\":\\"tag\\",\\"updated_at\\":\\"2026-10-12T06:08:39.591+08:00\\"},\\"complete_privileged_respo'
    b'nse\\":{\\"_links\\":{\\"html\\":{\\"href\\":\\"https://github.com/medtechcorps-netizen/whatomate/rules/'
    b'24896373\\"},\\"self\\":{\\"href\\":\\"https://api.github.com/repos/medtechcorps-netizen/whatomate/rul'
    b'esets/24896373\\"}},\\"bypass_actors\\":[],\\"conditions\\":{\\"ref_name\\":{\\"exclude\\":[],\\"include\\"'
    b':[\\"refs/tags/rereply-compat-negative-v1-*\\"]}},\\"created_at\\":\\"2026-10-12T06:08:39.569+08:00\\"'
    b',\\"current_user_can_bypass\\":\\"never\\",\\"enforcement\\":\\"active\\",\\"id\\":24896373,\\"name\\":\\"rer'
    b'eply-compat-negative-v1\\",\\"node_id\\":\\"RRS_lACqUmVwb3NpdG9yec5NU-iuzgF743U\\",\\"rules\\":[{\\"para'
    b'meters\\":{\\"update_allows_fetch_and_merge\\":false},\\"type\\":\\"update\\"},{\\"type\\":\\"deletion\\"}]'
    b',\\"source\\":\\"medtechcorps-netizen/whatomate\\",\\"source_type\\":\\"Repository\\",\\"target\\":\\"tag\\"'
    b',\\"updated_at\\":\\"2026-10-12T06:08:39.591+08:00\\"},\\"epoch\\":\\"rereply-compat-negative-v1\\",\\"fo'
    b'rmat_version\\":1,\\"observed_at\\":\\"2026-10-11T22:15:58.435908+00:00\\",\\"old_rule_unchanged\\":tru'
    b'e,\\"original_old_rule_response_sha256\\":\\"d5a27954a5a2ab001736f3ce94222f114e71ed5aed2462b6747b58'
    b'48bc97c761\\",\\"original_post_response_sha256\\":\\"15532a156ac0760104d3e00274c311bcbe0bd8759fbc305'
    b'9929cbb2a4980dbe4\\",\\"original_refused_result_sha256\\":\\"0d68c378622848706c32dc93ed00ad65b4a619c'
    b'de11f23265514597be42d5631\\",\\"prefix\\":\\"rereply-compat-negative-v1-\\",\\"privileged_read\\":true,'
    b'\\"qualification\\":\\"One actual POST, then read-only recovery of its exact captured rule ID. Conf'
    b'iguration only; publisher permissions, signed intent and outage admission remain unqualified.\\",'
    b'\\"repository\\":\\"medtechcorps-netizen/whatomate\\",\\"repository_id\\":1297344686,\\"request_sha256\\'
    b'":\\"82ab9132f26cea08c7398c32c3c37f3e218d1aad773e2248e04a0047a9304d6b\\",\\"rule\\":{\\"bypass_actors'
    b'\\":[],\\"conditions\\":{\\"ref_name\\":{\\"exclude\\":[],\\"include\\":[\\"refs/tags/rereply-compat-negat'
    b'ive-v1-*\\"]}},\\"created_at\\":\\"2026-10-11T22:08:39.569000Z\\",\\"enforcement\\":\\"active\\",\\"id\\":2'
    b'4896373,\\"name\\":\\"rereply-compat-negative-v1\\",\\"rules\\":[{\\"type\\":\\"deletion\\"},{\\"parameters'
    b'\\":{\\"update_allows_fetch_and_merge\\":false},\\"type\\":\\"update\\"}],\\"source\\":\\"medtechcorps-net'
    b'izen/whatomate\\",\\"source_type\\":\\"Repository\\",\\"target\\":\\"tag\\",\\"updated_at\\":\\"2026-10-11T2'
    b'2:08:39.591000Z\\"},\\"time_check\\":\\"Created and updated instants fall within the approximate ori'
    b'ginal execution window reconstructed from rounded elapsed seconds, with five-second clock tolera'
    b'nce; exact original strings and normalized instants retained.\\"}\\n"}\n'
)
_INSTALLED_CONSUMER_POLICY: bytes | None = None


class NegativeIndexRefusal(ValueError):
    def __init__(self, code="negative-index-invalid"):
        super().__init__(code if code in {"negative-index-invalid", "negative-index-unconfigured", "negative-index-changed"} else "negative-index-invalid")


@dataclass(frozen=True, slots=True)
class ProducerSetup:
    raw: bytes
    repository: str
    repository_id: int
    epoch: str
    prefix: str
    setup_witness: bytes
    setup_review_sha256: str
    rule_id: int
    created_at: dt.datetime
    updated_at: dt.datetime
    witness_observed_at: dt.datetime

    @property
    def sha256(self):
        return _digest(self.raw)


@dataclass(frozen=True, slots=True)
class SourceEntry:
    path: str
    mode: str
    byte_count: int
    sha256: str


@dataclass(frozen=True, slots=True)
class LineageEntry:
    producer_sha: str
    inventory: tuple[SourceEntry, ...]


@dataclass(frozen=True, slots=True)
class ConsumerPolicy:
    raw: bytes
    setup: ProducerSetup
    lineage: tuple[LineageEntry, ...]

    @property
    def sha256(self):
        return _digest(self.raw)


@dataclass(frozen=True, slots=True)
class PublicationRequest:
    producer_sha: str
    run_id: int
    run_attempt: int
    result_sha256: str
    context_sha256: str
    protocol: str


@dataclass(frozen=True, slots=True)
class NoIntentRequired:
    request: PublicationRequest
    setup_sha256: str


@dataclass(frozen=True, slots=True)
class IntentPersisted:
    request: PublicationRequest
    setup_sha256: str
    marker_ref: str


@dataclass(frozen=True, slots=True)
class RuleView:
    raw: bytes
    rule_id: int
    created_at: dt.datetime
    updated_at: dt.datetime
    bypass_visibility: str


@dataclass(frozen=True, slots=True)
class RefInventory:
    raw: bytes
    producer_sha: str
    rows: tuple[bytes, ...]


@dataclass(frozen=True, slots=True)
class HttpObservation:
    raw: bytes
    status: int
    state: str


@dataclass(frozen=True, slots=True)
class IndexSnapshot:
    setup_sha256: str
    producer_sha: str
    before_rule: RuleView
    before_refs: RefInventory
    after_rule: RuleView
    after_refs: RefInventory
    started_at: dt.datetime
    observed_at: dt.datetime


def _need(value):
    if not value:
        raise NegativeIndexRefusal()


def _digest(raw):
    _need(type(raw) is bytes)
    return hashlib.sha256(raw).hexdigest()


def _dump(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode("utf-8")


def _pairs(items):
    result = {}
    for key, value in items:
        _need(key not in result)
        result[key] = value
    return result


def _bad_number(_):
    raise NegativeIndexRefusal()


def _load(raw, maximum, *, canonical=False):
    _need(type(raw) is bytes and 0 < len(raw) <= maximum)
    text = raw.decode("utf-8", "strict")
    # Charge and bound depth before json.loads can recurse. This scan only
    # handles string escaping and structural depth; json.loads validates syntax.
    depth, quoted, escaped = 0, False, False
    for char in text:
        if quoted:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quoted = False
        elif char == '"':
            quoted = True
        elif char in "[{":
            depth += 1
            _need(depth <= MAX_DEPTH)
        elif char in "]}":
            depth -= 1
            _need(depth >= 0)
    _need(depth == 0 and not quoted)
    value = json.loads(text, object_pairs_hook=_pairs, parse_float=_bad_number, parse_constant=_bad_number)
    pending, count = [value], 0
    while pending:
        item = pending.pop()
        count += 1
        _need(count <= MAX_NODES)
        if type(item) is dict:
            pending.extend(item.keys()); pending.extend(item.values())
        elif type(item) is list:
            pending.extend(item)
        elif type(item) is str:
            _need(len(item.encode("utf-8")) <= MAX_CONFIG and "\0" not in item and "\ufffd" not in item
                  and not any(ord(c) < 32 and c not in "\r\n\t" or ord(c) == 127 for c in item))
        elif type(item) is int:
            _need(-(1 << 63) < item < (1 << 63))
        else:
            _need(item is None or type(item) is bool)
    if canonical:
        _need(raw == _dump(value))
    return value


def _object(value, names):
    _need(type(value) is dict and set(value) == set(names))
    return value


def _text(value, maximum=4096):
    _need(type(value) is str and 0 < len(value.encode()) <= maximum
          and all(32 <= ord(c) != 127 and c != "\ufffd" for c in value))
    return value


def _hex(value, pattern=_HASH):
    _need(type(value) is str and pattern.fullmatch(value) is not None)
    return value


def _positive(value, maximum=10**16):
    _need(type(value) is int and 0 < value < maximum)
    return value


def _utc(value):
    _need(type(value) is dt.datetime and value.tzinfo is dt.timezone.utc and value.fold == 0)
    return value


def _date(value):
    _need(type(value) is str and _TIME.fullmatch(value) is not None and not value.endswith("-00:00"))
    if not value.endswith("Z"):
        _need(int(value[-5:-3]) < 24 and int(value[-2:]) < 60)
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    return _utc(parsed.astimezone(dt.timezone.utc))


def _date_wire(value):
    return value.isoformat(timespec="microseconds").replace("+00:00", "Z")


def _same(left, right):
    if type(left) is not type(right):
        return False
    if type(left) is dt.datetime:
        return left.tzinfo is dt.timezone.utc and left.fold == 0 and left == right
    if is_dataclass(left):
        return all(_same(getattr(left, f.name), getattr(right, f.name)) for f in fields(right))
    if type(left) is tuple:
        return len(left) == len(right) and all(_same(a, b) for a, b in zip(left, right))
    return left == right


def _rule(value, epoch, *, hidden=False, normalized=False):
    required = {"id", "name", "target", "source_type", "source", "enforcement", "conditions", "rules", "created_at", "updated_at"}
    optional = {"bypass_actors"} if normalized else {"bypass_actors", "node_id", "current_user_can_bypass", "_links"}
    _need(type(value) is dict and required <= value.keys() and value.keys() <= required | optional)
    rule_id = _positive(value["id"])
    _need(value["name"] == epoch and value["target"] == "tag" and value["source_type"] == "Repository"
          and value["source"] == REPOSITORY and value["enforcement"] == "active")
    conditions = _object(value["conditions"], ("ref_name",))
    ref = _object(conditions["ref_name"], ("include", "exclude"))
    _need(ref["include"] == ["refs/tags/" + PREFIX + "*"] and type(ref["include"]) is list
          and type(ref["exclude"]) is list and not ref["exclude"])
    rules = value["rules"]
    _need(type(rules) is list and len(rules) == 2 and all(type(r) is dict for r in rules))
    by_type = {}
    for rule in rules:
        _need(type(rule.get("type")) is str and rule["type"] not in by_type)
        by_type[rule["type"]] = rule
    _need(set(by_type) == {"update", "deletion"})
    _object(by_type["deletion"], ("type",))
    update = _object(by_type["update"], ("type", "parameters"))
    params = _object(update["parameters"], ("update_allows_fetch_and_merge",))
    _need(params["update_allows_fetch_and_merge"] is False)
    if "bypass_actors" in value:
        _need(type(value["bypass_actors"]) is list and not value["bypass_actors"])
        visibility = "returned-empty"
    else:
        _need(hidden)
        visibility = "hidden-under-setup-trust"
    if "node_id" in value:
        _need(re.fullmatch(r"[A-Za-z0-9_=-]{1,256}", _text(value["node_id"], 256)) is not None)
    if "current_user_can_bypass" in value:
        _need(value["current_user_can_bypass"] == "never")
    if "_links" in value:
        links = _object(value["_links"], ("self", "html"))
        for name, url in (("self", "https://api.github.com" + API + "/rulesets/" + str(rule_id)),
                          ("html", "https://github.com/" + REPOSITORY + "/rules/" + str(rule_id))):
            _need(_object(links[name], ("href",))["href"] == url)
    created, updated = _date(value["created_at"]), _date(value["updated_at"])
    _need(created <= updated)
    if normalized:
        _need(value["created_at"] == _date_wire(created) and value["updated_at"] == _date_wire(updated)
              and [rule["type"] for rule in rules] == ["deletion", "update"])
    return rule_id, created, updated, visibility


def _parse_setup(raw):
    value = _object(_load(raw, MAX_CONFIG, canonical=True), ("format_version", "repository", "repository_id", "epoch", "prefix", "setup_witness", "setup_review_sha256"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1
          and value["repository"] == REPOSITORY and type(value["repository_id"]) is int
          and value["repository_id"] == REPOSITORY_ID and value["prefix"] == PREFIX
          and type(value["epoch"]) is str and _EPOCH.fullmatch(value["epoch"]) is not None)
    _hex(value["setup_review_sha256"])
    _need(type(value["setup_witness"]) is str)
    witness_raw = value["setup_witness"].encode("utf-8")
    witness = _object(_load(witness_raw, MAX_WITNESS, canonical=True), (
        "complete_original_post_response", "complete_privileged_response", "epoch", "format_version", "observed_at", "old_rule_unchanged",
        "original_old_rule_response_sha256", "original_post_response_sha256", "original_refused_result_sha256", "prefix", "privileged_read",
        "qualification", "repository", "repository_id", "request_sha256", "rule", "time_check"))
    _need(type(witness["format_version"]) is int and witness["format_version"] == 1
          and witness["repository"] == REPOSITORY and type(witness["repository_id"]) is int
          and witness["repository_id"] == REPOSITORY_ID and witness["prefix"] == PREFIX
          and witness["epoch"] == value["epoch"] and witness["privileged_read"] is True and witness["old_rule_unchanged"] is True)
    for name in ("original_old_rule_response_sha256", "original_post_response_sha256", "original_refused_result_sha256", "request_sha256"):
        _hex(witness[name])
    _text(witness["qualification"]); _text(witness["time_check"])
    original = _rule(witness["complete_original_post_response"], value["epoch"])
    privileged = _rule(witness["complete_privileged_response"], value["epoch"])
    normalized = _rule(witness["rule"], value["epoch"], normalized=True)
    _need(original == privileged == normalized and _digest(_dump(witness["complete_original_post_response"])) == witness["original_post_response_sha256"])
    observed = _date(witness["observed_at"])
    _need(original[2] <= observed)
    return ProducerSetup(raw, REPOSITORY, REPOSITORY_ID, value["epoch"], PREFIX, witness_raw,
                         value["setup_review_sha256"], *original[:3], observed)


def _safe(call, *args, **kwargs):
    try:
        return call(*args, **kwargs)
    except NegativeIndexRefusal:
        raise
    except (ValueError, TypeError, KeyError, AttributeError, UnicodeError, OverflowError, RecursionError):
        raise NegativeIndexRefusal() from None


def parse_producer_setup(raw):
    return _safe(_parse_setup, raw)


def _valid_setup(setup):
    _need(type(setup) is ProducerSetup and _same(setup, parse_producer_setup(setup.raw)))
    return setup


def _path(value):
    _text(value, 256)
    _need(not value.startswith("/") and re.fullmatch(r"[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*", value) is not None
          and all(part not in {".", ".."} for part in value.split("/")))
    return value


def _parse_consumer(raw):
    value = _object(_load(raw, MAX_CONFIG, canonical=True), ("format_version", "producer_setup", "lineage"))
    _need(type(value["format_version"]) is int and value["format_version"] == 1 and type(value["producer_setup"]) is str)
    setup = parse_producer_setup(value["producer_setup"].encode())
    _need(type(value["lineage"]) is list and 0 < len(value["lineage"]) <= MAX_LINEAGE)
    lineages, count, total = [], 0, 0
    for item in value["lineage"]:
        _object(item, ("producer_sha", "inventory")); _hex(item["producer_sha"], _SHA)
        _need(type(item["inventory"]) is list and 0 < len(item["inventory"]) <= MAX_INVENTORY)
        entries = []
        for row in item["inventory"]:
            _object(row, ("path", "mode", "bytes", "sha256"))
            _path(row["path"]); _hex(row["sha256"])
            _need(type(row["mode"]) is str and row["mode"] in {"100644", "100755"})
            _positive(row["bytes"], MAX_SOURCE+1)
            count += 1; total += row["bytes"]
            _need(count <= MAX_INVENTORY and total <= MAX_SOURCE_TOTAL)
            entries.append(SourceEntry(row["path"], row["mode"], row["bytes"], row["sha256"]))
        names = tuple(e.path for e in entries)
        _need(names == tuple(sorted(set(names))))
        lineages.append(LineageEntry(item["producer_sha"], tuple(entries)))
    heads = tuple(e.producer_sha for e in lineages)
    _need(heads == tuple(sorted(set(heads))))
    return ConsumerPolicy(raw, setup, tuple(lineages))


def parse_consumer_policy(raw):
    return _safe(_parse_consumer, raw)


def load_producer_setup():
    if _INSTALLED_PRODUCER_SETUP is None:
        raise NegativeIndexRefusal("negative-index-unconfigured")
    return parse_producer_setup(_INSTALLED_PRODUCER_SETUP)


def load_consumer_policy():
    if _INSTALLED_CONSUMER_POLICY is None:
        raise NegativeIndexRefusal("negative-index-unconfigured")
    return parse_consumer_policy(_INSTALLED_CONSUMER_POLICY)


def _validate_request(request):
    _need(type(request) is PublicationRequest)
    _hex(request.producer_sha, _SHA); _positive(request.run_id); _positive(request.run_attempt, 10000)
    _hex(request.result_sha256); _hex(request.context_sha256)
    _need(type(request.protocol) is str and request.protocol in {"normal3", "rollback4"})
    return request


def validate_publication_request(request):
    return _safe(_validate_request, request)


def marker_ref(request):
    validate_publication_request(request)
    return "refs/tags/" + PREFIX + f"{request.producer_sha}-{request.run_id}-{request.run_attempt}-{request.result_sha256}-{request.context_sha256}"


def _replay_rule(raw, setup):
    _valid_setup(setup)
    rule_id, created, updated, visibility = _rule(_load(raw, MAX_BODY), setup.epoch, hidden=True)
    _need((rule_id, created, updated) == (setup.rule_id, setup.created_at, setup.updated_at))
    return RuleView(raw, rule_id, created, updated, visibility)


def replay_rule(raw, setup):
    return _safe(_replay_rule, raw, setup)


def _replay_refs(raw, producer_sha):
    _hex(producer_sha, _SHA)
    value = _load(raw, MAX_BODY)
    _need(type(value) is list and len(value) <= MAX_REFS)
    # Never filter rows by purported marker grammar or object SHA. Every row
    # from the concrete exact-H query is retained as a permanent intent veto.
    return RefInventory(raw, producer_sha, tuple(_dump(row) for row in value))


def replay_refs(raw, *, producer_sha):
    return _safe(_replay_refs, raw, producer_sha)


def _http_frame(raw):
    """Bounded framing only, no endpoint/status or completed-capture authority."""
    return _safe(_frame, raw)


def _frame(raw):
    _need(type(raw) is bytes and 0 < len(raw) <= MAX_HEADERS+MAX_BODY)
    delimiter = b"\r\n\r\n" if b"\r\n\r\n" in raw else b"\n\n"
    head, separator, body = raw.partition(delimiter)
    _need(separator and 0 < len(head) <= MAX_HEADERS and len(body) <= MAX_BODY)
    newline = b"\r\n" if delimiter == b"\r\n\r\n" else b"\n"
    lines = head.split(newline)
    # gh --include writes the status with LF and serializes headers with CRLF.
    # Split only that status boundary; every header and the body stay exact.
    if newline == b"\r\n" and b"\n" in lines[0]:
        lines = lines[0].split(b"\n", 1) + lines[1:]
    match = re.fullmatch(rb"HTTP/(?:1\.1|2(?:\.0)?) ([0-9]{3})(?: [ -~]{0,64})?", lines[0])
    _need(match is not None and len(lines) <= 129)
    status, headers = int(match[1]), {}
    for line in lines[1:]:
        _need(re.fullmatch(rb"[!#$%&'*+.^_`|~0-9A-Za-z-]+:[ \t]*[ -~]*", line) is not None)
        key, value = line.split(b":", 1); key = key.lower()
        _need(key not in headers)
        headers[key] = value.strip()
    _need(b"location" not in headers and b"transfer-encoding" not in headers
          and not body.startswith(b"HTTP/") and b"\x00" not in body
          and re.search(rb"(?:\A|[\r\n])[ \t]*HTTP/[0-9]", body) is None)
    if b"content-length" in headers:
        _need(re.fullmatch(rb"0|[1-9][0-9]{0,5}", headers[b"content-length"]) is not None
              and int(headers[b"content-length"]) == len(body))
    return status, body, headers


def _replay_http(raw, endpoint):
    _need(type(endpoint) is str and endpoint == ACTIONS_ENDPOINT)
    status, body, headers = _http_frame(raw)
    _need(status in {200, 502, 503, 504})
    if status == 200:
        _need(headers.get(b"content-type", b"").lower() in {b"application/json", b"application/json; charset=utf-8"})
        value = _object(_load(body, MAX_BODY), ("total_count", "workflow_runs"))
        _need(type(value["total_count"]) is int and 0 <= value["total_count"] < 10**16
              and type(value["workflow_runs"]) is list and len(value["workflow_runs"]) <= 1
              and value["total_count"] >= len(value["workflow_runs"]) and all(type(row) is dict for row in value["workflow_runs"]))
    return HttpObservation(raw, status, "available" if status == 200 else "unavailable")


def replay_http(raw, *, endpoint):
    return _safe(_replay_http, raw, endpoint)


def _replay_index(setup, before_rule, before_refs, after_rule, after_refs, producer_sha, started_at, observed_at):
    _valid_setup(setup); _utc(started_at); _utc(observed_at)
    _need(started_at <= observed_at)
    br, ar = replay_rule(before_rule, setup), replay_rule(after_rule, setup)
    bf, af = replay_refs(before_refs, producer_sha=producer_sha), replay_refs(after_refs, producer_sha=producer_sha)
    if before_rule != after_rule or before_refs != after_refs:
        raise NegativeIndexRefusal("negative-index-changed")
    return IndexSnapshot(setup.sha256, producer_sha, br, bf, ar, af, started_at, observed_at)


def replay_index(setup, before_rule, before_refs, after_rule, after_refs, *, producer_sha, started_at, observed_at):
    return _safe(_replay_index, setup, before_rule, before_refs, after_rule, after_refs, producer_sha, started_at, observed_at)
