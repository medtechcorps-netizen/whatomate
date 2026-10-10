# Compatibility proof core

`release/compat/proof.py` is a pure standard-library evidence evaluator. It has
no Docker, GitHub, filesystem, network or database operations. Its synthetic
tests exercise proof rules; they do **not** establish compatibility of a release.
The full compatibility experiment, workflow, result attestation, release preview
and real runs are still unimplemented.

## Result and failure contract

`evaluate_proof(observations)` returns exactly these public fields:

```text
sha, previous_release, previous_web_digest, classification, forward,
old_migrate_ok {rw, ro, negative_control},
old_server_ok {boot, read, write, reverse, workers},
reversible, catalog_sha256, run_id, run_attempt
```

`classification` contains only `{kind, dormant}`. Current classifier outputs
are `none/false` and `data/true`; future `expand` (with either dormant value) and
`contract/false` are representable for their planned proof rules. This module
does not make or authorize a classification. The runner must bind the actual
verified classifier output, including rejection reasons, before evaluating a
proof. It must not substitute a more permissive kind. Source paths/reasons and
summaries are not copied into the public result.

Only **completed semantic failures** yield false flags. Infrastructure failures
(pull, network, boot timeout, capture, container or process failure) raise
`InfrastructureFailure` with a fixed code, and there is no result. Missing,
not-run, incomplete, malformed or ambiguous evidence raises `EvidenceError`.
A boot without its first log is infrastructure failure. A control whose
failure cannot be proved also raises; it is never a favorable partial result.
Every phase is validated even when an earlier semantic check fails.
In particular, a real semantic boot failure followed by unrun dependent sweeps
fails the job as incomplete evidence in this version. The runner must not
fabricate completed false sweep rows. There is no semantic-blocked encoding.

`reversible` is derived solely from all three `old_migrate_ok` values. It is
not derived from forward or old-server results. Those other results remain
separate mandatory acceptance considerations for the later release policy.

`result_bytes` validates the exact shape and produces canonical sorted JSON
plus a newline. Public strings are limited to lowercase source/digest hex,
the release tag grammar, and bounded classification values. The only numeric
identity is the explicitly required GitHub run ID; run attempt is also
included. No table names, SQL, UUIDs, hostnames, local paths or raw log messages
are exported. Errors never include input values. `artifact_name` returns
`compat-result-<sha>-<run_attempt>` and accepts rerun attempts greater than one.
`validate_rollback_target` accepts the existing release tag regex or
`prod-0000`, with full-string matching.

## Structured observations

The exact required input keys are defined by `evaluate_proof` and demonstrated
by `observations()` in `test_proof.py`. Unknown keys refuse. The reusable shapes
are:

- Execution: `{status, exit_code, failure}`. Status must be `completed` with
  exit code 0–255 and failure `null`, or `infrastructure-error` with a fixed
  failure category. `not-run` or missing execution cannot establish a flag.
- Check: `{execution, passed}`. Only a successful completed execution and the
  explicit boolean `true` pass. Sweep observations add the exact six booleans
  `contacts`, `inbound`, `outbound`, `template`, `flow`, `settings` instead of a
  single passed flag.
- Table capture: `{complete: true, digests: {table_identity_sha256: data_sha256}}`.
  The runner must hash a deterministic, collision-resistant identity and data
  encoding, enumerate **every** table in the measured set and include empty
  tables. Missing/deleted entries are not an unchanged table. This core does
  not invent a row digest algorithm or query tenant data.
- Log window: `{raw: bytes, complete: true}` for a single designated
  application and phase. Markers are `{complete: true, application, hashes}`;
  their hashes bind canonical probe events selected by the future runner.
- Migration: `{execution, log, markers, catalog_sha256, tables}`.

The top-level observations include the previous server boot/startup proof,
the sorted unique nonempty `expected_preexisting_tables` identity hashes from
the verified base catalog, before-forward table capture, target catalog, two forward passes, candidate
readiness/write sweep, stopped-candidate and zero-client quiescence with D0,
both N-1 migrations and the read-only control, all old-server sweeps/workers,
and the final catalog observation. They must come from one correctly ordered
experiment on the bound source and previous image. The evaluator cannot prove
collection completeness, provenance, ordering or signatures from booleans
and hashes alone; the future runner is responsible for measuring and binding
those facts, rather than fabricating defaults when a phase did not run.
The before-capture keys must exactly equal that expected inventory; a vacuous
or omitted-table baseline refuses. D0 must retain the expected identities and
have exactly the post-forward table inventory. The independently established
expected inventory must not be derived from a possibly incomplete capture.

Both forward passes must succeed and equal the declared target catalog. Every
pre-existing table digest must remain unchanged for `none`, `expand`, or any
`dormant` classification. Additional tables may exist for an expansion.
`none` and dormant classes allow no mod statements or probe markers.
For those no-mod classes, each pass must also have exactly the before-table
inventory: an unknown extra table cannot hide behind a known-object catalog
SHA. Active expansions retain the allowed additional-table behavior. Other
classes require every observed mod/marker hash to appear in the explicit
classification-authorized hash sets. This is an exact authorization interface,
**not** an SQL parser or a derivation of which objects a migration may touch.
The later runner must derive those sets from the reviewed classification and
structured probe records; it cannot authorize whatever it just observed.

N-1 read-write and read-only results both require exit zero, no parsed errors,
no mod statements, no markers, the same D0 table inventory/digests and the
same catalog. A swallowed UPDATE with exit zero and unchanged data still fails
through its mod log or marker. A caught `25006` still fails the old read-only
migration even if its process exits zero.

The independent control must execute the exact fixed statement
`CREATE TABLE compat_probe.read_only_control (sentinel integer)` in the
read-only session. It must exit nonzero, report SQLSTATE `25006`, and its log
must contain exactly one `25006` primary error with that same diagnostic
STATEMENT. An optional matching mod-log attempt is allowed; a different query,
successful control, extra error, unrelated write or absent parser evidence
refuses the experiment. The probe schema stays outside `public`.

Previous/old server boot requires readiness and an observed startup contract.
If that contract line is unavailable, the structured fallback must prove
FORCE RLS was removed and the negative-control server refused to start.
The old-server read/write sweeps and candidate reverse sweep include all six
cases. Workers need at least 60 observed seconds with no panic, unknown-type
line or unexpected stub journal entry. A short successful window is incomplete;
an observed early crash is a semantic failure. The counts must cover the
relevant old-server observation period, not discard errors outside a selected
tail. The previous server must stop before candidate restart/reverse sweep.

## Narrow PostgreSQL log parser

`parse_pg_log` accepts only a **complete captured proof window**, already
bounded by the runner and assigned to one of:

```text
compat-prev-server  compat-cand-server  compat-fwd
compat-n1-rw        compat-n1-ro        compat-control
```

The required format is PostgreSQL text stderr with `log_timezone=UTC`, `lc_messages=C`,
`log_error_verbosity=verbose`, and `log_line_prefix='%m [%p] %a %e '`.
The runner must also set `log_statement=mod`,
`log_min_error_statement=error`, and disable autovacuum for the experiment.
This parser does not validate arbitrary PostgreSQL stderr. Startup/background
lines with a blank application name are outside the client proof window; the
runner must not silently discard an unattributed line inside that window.
Alongside the synthetic cases, a narrow local check against the pinned
PostgreSQL 17 image parsed three actual log forms: a multiline UPDATE, an
extended-protocol unnamed UPDATE, and the read-only CREATE control failing
with SQLSTATE `25006`. That check validates only those formats. Complete
proof-window collection, the full runner and release compatibility remain
unverified.

Primary VERBOSE records include the repeated SQLSTATE after the severity.
Following STATEMENT/LOCATION/DETAIL/HINT fields must belong to the same
PID/application/SQLSTATE. ERROR + STATEMENT + LOCATION is one error, not an
extra executed mod statement. A `LOG: ... statement:` or
`LOG: ... execute <unnamed>:` / simple named prepared-statement envelope counts
once and is hashed in full. A tab-prefixed physical continuation belongs to
the preceding field, so a multiline UPDATE counts once. A second prefix
hidden under a tab, orphan/unprefixed line, duplicate detail, mismatched
application, malformed SQLSTATE or truncated final line refuses.

Function CONTEXT traces, SELECT/DO/CALL as mod-statement envelopes, unexpected
LOG forms and other unsupported statement envelopes refuse rather than infer
nested effects. This is a conservative parser limitation, not a claim that
PostgreSQL logs every possible function side effect. Complete table digests
and DDL probe markers remain independent evidence. SQL is retained only long
enough to hash the statement; it never reaches the public result or errors.

The format references are the PostgreSQL 17 [logging settings](https://www.postgresql.org/docs/17/runtime-config-logging.html)
and [server error output implementation](https://github.com/postgres/postgres/blob/REL_17_STABLE/src/backend/utils/error/elog.c).
The latter prefixes LOCATION and STATEMENT separately and uses tab-prefixed
continuations. The former also documents mod logging for forms such as
COPY FROM, PREPARE/EXECUTE and EXPLAIN ANALYZE. Unsupported forms here remain
explicit refusals, not silently ignored statements. The exact locale/timezone
settings are required; these references do not replace a real PG17 integration
test of the future collector and parser together.

Limits are 8 MiB, 100,000 lines, 64 KiB per physical line and 10,000 statements
or errors per log; table/marker inventories are also bounded. The evaluator
accepts five such migration/control windows. An explicitly complete empty
window is valid under mod-only logging, but does not replace successful
execution evidence or the mandatory independently observed control failure.

## Bounded runtime foundation

`runtime.py` supplies a small CLI foundation for the future runner. It drains
stdout and stderr concurrently with separate byte limits, imposes a deadline,
and reaps its child process. A completed nonzero exit is captured as evidence;
it is not automatically a compatibility verdict. Process or capture failures
raise fixed infrastructure errors without exposing arguments or stream data.
On POSIX, capture failure kills the private process group. On Windows, only
the direct child is killed; container cleanup remains the runner's separate
responsibility.

The Docker interface supports only digest-pinned PostgreSQL, Redis and verified
previous-web pulls/inspection for Linux AMD64. It uses a fresh empty config and
the default local context. The GitHub interface reuses the existing release
record, chain, exact certificate and image attestation validators. It bounds
read-only output and creates manifest files only in fresh scratch directories;
it cannot publish releases or overwrite/delete earlier evidence. Its caller
supplies reviewed executable paths, an explicit minimal environment and the
existing read-only job token in memory. It does not retrieve credentials.

The focused transport tests include real local Python child processes, while
Docker and GitHub are mocked. They establish the transport behavior, not a
live registry pull, a current signed release, or release compatibility. No
SQL collector, container lifecycle, sweep, workflow or publication is added by
this foundation.

## Remaining runner and workflow work

The later PR8 runner must resolve and verify the previous record/image,
validate source and classifier bindings, create the internal Docker network,
prepare the PG17 synthetic cluster, collect the complete observations above,
and publish/attest the result. It must keep infrastructure failures distinct
from semantic compatibility results. No workflow or runner is added here.

Use the existing `release/staging/graphstub` and its separate Dockerfile, plus
the current meta-relay Graph-base configuration; do not recreate their old
proposed package paths. Existing read jobs request `attestations: read` as
well as contents/actions read. The release workflow tests anonymous exact
image pulls, but this core does not establish current registry visibility.

Until the previous released image supports `rls-bootstrap-empty`, the planned
fallback is `TestCompatBootstrapExport` with equal base/head golden and the
base production overlay. Its DSN is strictly loopback-only: put the exporter
in a compatible loopback/container network topology instead of loosening that
guard for a Docker service name. PR7 remains constrained by the conservative
migration closure, independently of its dormant entrypoint exemption.

PR4's production observation/overlay remains a prerequisite. PG17 synthetic
validation does not establish the actual production catalog or equivalence
with supported PG14. Compare the original
O-1 hash with the same golden-only known identities, separately check the
overlay-known synthetic inventory, and separately account for accepted
unpublished production objects. None of these measurements runs here.

Future hosted acceptance still requires the planned PR run, three consecutive
green main compatibility runs, and the swallowed-UPDATE advisory case before
the compatibility-backed classifier lift. Synthetic unit tests here do not
replace those outcomes or any existing release approval boundary.
