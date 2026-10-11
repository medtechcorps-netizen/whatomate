"""Source-bound reviewed-v0 catalog observations, not a catalog normalizer.

The caller first verifies the previous signed release using runtime.verified_previous.
This module then reads fixed paths from its immutable Git commit and candidate
commit; working-tree files cannot supply the expected table inventory. The
initial e001 baseline has no overlay. Later verified releases must contain the
same pinned v0 artifacts as the candidate, including its physical-order profile
and comments-only NOP overlay. Other catalog progression refuses rather than
applying arbitrary head DDL to an old release.

Catalog SHA values come only from the actual owned candidate Go CLI. The equal
embedded golden contract lets that CLI observe every current phase against the
same identities. No owner credentials, unknown names, seed values or raw logs
are public outputs. Docker/Go/DB operations never execute at import.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import postgres
import processes

runtime, proof = processes.runtime, processes.proof
_BASE = "e0016b3595114728e7864da2adb44c29065f25ef"
_GOLDEN = "internal/dbcatalog/golden/catalog.json"
_PROFILE = "internal/dbcatalog/golden/production-v0.json"
_OVERLAY = "internal/dbcatalog/shape/production-v0.sql"
_PINS = {
    _GOLDEN: "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8",
    "internal/dbcatalog/golden/global_tables.json": "c125e9283e3eaa80ad5d2cd86a0ee1236b4a71884a59907329b29c4dd3a70ae3",
    "internal/dbcatalog/golden/history.json": "9b9a31ec3e2164031abb04ea271fb70a32207fd84528bc7faaea8e22f9864513",
    _PROFILE: "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d",
    _OVERLAY: "48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d",
}
_COUNT_KEYS = {"non_public_schemas", "non_public_objects", "other_roles", "default_acls", "oids", "reloptions",
               "statistics", "tablespaces", "other_acl_entries", "other_policy_roles"}
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_SHA = re.compile(r"[0-9a-f]{40}\Z")
# Only the CLI's fixed public diagnostics may refine a refusal. Never echo
# arbitrary process output: it may contain configuration or driver details.
_CLI_FAILURES = {
    (2, b"catalog-snapshot:invalid-arguments; use -help\n"): "catalog-cli-invalid-arguments",
    (1, b"catalog-snapshot:configuration-unavailable\n"): "catalog-cli-configuration-unavailable",
    (1, b"catalog-snapshot:capture-failed\n"): "catalog-cli-capture-failed",
    (1, b"catalog-snapshot:output-failed\n"): "catalog-cli-output-failed",
}


def need(value, code="catalog-contract-invalid"):
    if not value:
        raise proof.EvidenceError(code)


def _json(raw):
    need(type(raw) is bytes and 0 < len(raw) <= 4*runtime.MIB)
    try:
        return runtime.common.loads_strict(raw)
    except runtime.common.ReleaseError:
        raise proof.EvidenceError("catalog-json-invalid") from None


class CatalogContract:
    def __init__(self, repo: Path, previous: runtime.release_record.Entry, candidate_sha: str,
                 *, base_env=None, capture=runtime.bounded_command):
        need(isinstance(repo, Path) and repo.is_dir() and type(previous) is runtime.release_record.Entry)
        need(type(previous.sha) is str and _SHA.fullmatch(previous.sha)
             and type(candidate_sha) is str and _SHA.fullmatch(candidate_sha),
             "catalog-progression-unsupported")
        self.previous_sha, self.candidate_sha = previous.sha, candidate_sha
        digest = previous.images.get("web")
        need(type(digest) is str and runtime.common.DIGEST_RE.fullmatch(digest))
        self.previous_web = runtime.common.IMAGE_REPOSITORY["web"] + "@" + digest
        env = runtime.common.scrubbed_env({"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"}, base=base_env)
        def git(args, limit=4*runtime.MIB):
            try:
                result = capture(["git", "-C", str(repo), *args], env=dict(env), timeout=30,
                                 stdout_limit=limit, stderr_limit=runtime.MIB)
            except Exception:
                raise proof.InfrastructureFailure("catalog-source") from None
            need(type(result) is runtime.Capture and type(result.returncode) is int and result.returncode == 0
                 and type(result.stdout) is bytes and len(result.stdout) <= limit and not result.stderr,
                 "catalog-source-invalid")
            return result.stdout
        for sha in (self.previous_sha, candidate_sha):
            need(git(["cat-file", "-t", sha],1024) == b"commit\n", "catalog-source-invalid")
        git(["merge-base", "--is-ancestor", self.previous_sha, candidate_sha],1024)
        blobs = {}
        for name in _PINS:
            raw = git(["cat-file", "blob", candidate_sha+":"+name])
            need(hashlib.sha256(raw).hexdigest() == _PINS[name], "catalog-progression-unsupported")
            blobs[name] = raw
            if previous.sha != _BASE or name not in {_PROFILE,_OVERLAY}:
                need(git(["cat-file", "blob", self.previous_sha+":"+name]) == raw, "catalog-fallback-not-equal")
            else:
                # Absence is admitted only for the fixed first baseline; no
                # generic missing-overlay fallback is inferred for later refs.
                need(git(["ls-tree", "-z", self.previous_sha, "--", name],1024) == b"", "catalog-base-overlay-unsupported")
        self._base_overlay = ("explicit-empty-first-baseline" if previous.sha == _BASE
                              else "equal-reviewed-v0-nop")
        golden, profile = _json(blobs[_GOLDEN]), _json(blobs[_PROFILE])
        need(type(golden) is dict and set(golden) == {"version","objects"} and golden["version"] == 0)
        need(profile["released_source"] == _BASE and profile["golden_sha256"] == _PINS[_GOLDEN]
             and profile["accepted_unpublished"] == [] and _HASH.fullmatch(profile["live_sha256"]))
        need(all(not line.strip() or line.lstrip().startswith(b"--") for line in blobs[_OVERLAY].splitlines()))
        self.target_sha256 = profile["live_sha256"]
        self._known = frozenset((o["kind"],o["identity"]) for o in golden["objects"])
        self._relations = frozenset(o["identity"] for o in golden["objects"] if o["kind"]=="relations")
        self._kinds = frozenset(o["kind"] for o in golden["objects"])
        tables = [o for o in golden["objects"] if o["kind"]=="relations" and o["attributes"]["relkind"] in {"r","p","m","f"}]
        need(tables and all(o["attributes"]["relkind"]!="f" for o in tables))
        self.expected_preexisting_tables = tuple(sorted(postgres.identity(o["identity"]) for o in tables))
        need(len(set(self.expected_preexisting_tables)) == len(tables))
        self._seal = object()
        self._pins = dict(_PINS)

    def check_initial_tables(self, tables: postgres.TableCapture):
        need(type(tables) is postgres.TableCapture and tables._all_columns
             and tables.identities() == list(self.expected_preexisting_tables), "catalog-table-inventory-mismatch")

    def parse_compare(self, raw):
        need(type(raw) is bytes and 0<len(raw)<=runtime.MIB and raw.endswith(b"\n"), "catalog-output-invalid")
        value = _json(raw)
        need(type(value) is dict and set(value)=={"sha256","match","differences","unknown","count_only","ownership"}, "catalog-output-invalid")
        need(type(value["sha256"]) is str and _HASH.fullmatch(value["sha256"])
             and type(value["match"]) is bool and type(value["differences"]) is list, "catalog-output-invalid")
        seen=set()
        for row in value["differences"]:
            need(type(row) is dict and set(row)=={"kind","identity","status"}
                 and type(row["kind"]) is str and type(row["identity"]) is str, "catalog-output-invalid")
            key=(row["kind"],row["identity"])
            need(key in self._known and key not in seen and type(row["status"]) is str and row["status"] in {"changed","missing"}, "catalog-output-invalid")
            seen.add(key)
        need(value["match"] is (not seen), "catalog-output-invalid")
        # The reviewed v0 overlay admits no unpublished identities. A known-only
        # SHA cannot hide new unknown functions, relations or other objects.
        need(value["unknown"] == [] and type(value["unknown"]) is list, "catalog-unknown-unsupported")
        counts=value["count_only"]
        need(type(counts) is dict and set(counts)==_COUNT_KEYS and all(type(v) is int and 0<=v<2**63 for v in counts.values()), "catalog-output-invalid")
        ownership=value["ownership"]
        need(type(ownership) is dict and set(ownership)=={"migration_owner_owns_database","migration_owner_owns_public_schema"}
             and all(v is True for v in ownership.values()), "catalog-ownership-invalid")
        return value["sha256"]

    def public(self):
        return {"previous_sha":self.previous_sha,"candidate_sha":self.candidate_sha,
                "source_sha256":dict(self._pins),"base_overlay":self._base_overlay,
                "target_catalog_sha256":self.target_sha256,
                "expected_preexisting_tables":list(self.expected_preexisting_tables)}


@dataclass(frozen=True)
class CatalogObservation:
    sha256: str
    stdout_sha256: str
    stdout_bytes: int


class OwnedCatalog:
    def __init__(self, manager: processes.OwnedProcesses, pg: postgres.SyntheticPostgres, contract: CatalogContract):
        need(type(manager) is processes.OwnedProcesses and type(pg) is postgres.SyntheticPostgres and type(contract) is CatalogContract)
        need(manager.bindings["previous"].source_sha==contract.previous_sha
             and manager.bindings["previous"].reference==contract.previous_web
             and manager.bindings["candidate"].source_sha==contract.candidate_sha, "catalog-image-source-mismatch")
        need(pg.container==manager.owner._ids["pg"] and pg.token==manager.owner.token and pg.runtime_role=="compat_runtime",
             "catalog-pg-binding-invalid")
        self.manager,self.pg,self.contract=manager,pg,contract
        self._failed=False
        self._baseline=None
        self._initial_attempted=False

    def _quiescent(self):
        need(not self._failed and not self.manager._protocol_failed and self.pg._phase is None
             and not self.manager._ids, "catalog-phase-active")
        self.manager._assert_all()
        self.pg._check()
        result=self.pg._sql(postgres._READ_HEADER+self.pg._state_sql()+"COMMIT;\n")
        need(not result.stderr, "catalog-quiescence-diagnostics")
        rows=postgres._json_lines(result.stdout)
        need(len(rows)==1)
        self.pg._state(rows[0])

    def observe(self, inputs: processes.ConfigInputs) -> CatalogObservation:
        self._quiescent()
        try:
            handle=self.manager.start_catalog(inputs)
            completed=self.manager.wait_catalog(handle)
            # Known cleanup never turns a failed/ambiguous report into success.
            self.manager.remove(handle)
            if type(completed.returncode) is not int or completed.returncode != 0 or completed.stderr:
                code = "catalog-cli-failed"
                if type(completed.returncode) is int and type(completed.stderr) is bytes:
                    code = _CLI_FAILURES.get((completed.returncode, completed.stderr), code)
                raise proof.EvidenceError(code)
            observed=self.contract.parse_compare(completed.stdout)
            self._quiescent()
            return CatalogObservation(observed,hashlib.sha256(completed.stdout).hexdigest(),len(completed.stdout))
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise

    def initial(self, inputs):
        """Observed CLI then complete data capture, checked against source inventory."""
        need(not self._initial_attempted, "catalog-baseline-already-attempted")
        self._initial_attempted=True
        try:
            observed=self.observe(inputs)
            need(observed.sha256==self.contract.target_sha256, "catalog-baseline-mismatch")
            tables=self.pg.capture_tables()
            self.contract.check_initial_tables(tables)
            self._quiescent()
            self._baseline=tables
            return observed,tables
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise

    def after_phase(self, inputs, *, original_columns=False):
        """Caller must have terminated and finished its PG phase first.

        original_columns=True is forward comparison; False is all-column D0
        and N-1. Complete current tables remain present in both projections.
        """
        need(type(original_columns) is bool and self._baseline is not None, "catalog-baseline-missing")
        try:
            observed=self.observe(inputs)
            tables=self.pg.capture_tables(projection=self._baseline if original_columns else None)
            self._quiescent()
            return observed,tables
        except Exception:
            self._failed=True
            self.manager._protocol_failed=True
            raise
