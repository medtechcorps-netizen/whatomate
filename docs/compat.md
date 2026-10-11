# Compatibility experiment and proof

`release/compat/proof.py` is a pure standard-library evidence evaluator. It has
no Docker, GitHub, filesystem, network or database operations. Its synthetic
tests exercise proof rules; they do **not** establish compatibility of a release.
The ordered experiment, source-built image admission and hosted entry points
are implemented. The advisory workflow is prepared locally, and execution
remains disabled pending the complete synthetic validation described below.
Component tests do not establish release compatibility.

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
(pull, network, boot timeout, capture or unclassified container/process failure)
refuse with fixed error codes and no result. Missing, not-run, incomplete,
malformed or ambiguous evidence raises `EvidenceError`.
A boot without its first log is infrastructure failure. A control whose
failure cannot be proved also raises; it is never a favorable partial result.
Every phase is validated even when an earlier semantic check fails.

One narrow postmigration terminal case can causally block dependent phases.
After the actual migration and independent read-only control observations,
`start_web` must return the bound previous-server handle. Complete append-only
logs must contain the source's start and RLS markers before an anchored Go
`panic:` in the same stream, with exit 2 and stable exited, non-OOM state.
Structured fatal records, runtime `fatal error:` lines (including mixed panic
windows), pre-RLS failures and missing or ambiguous evidence still refuse.
The private proof then binds blocked old read/write/workers and reverse
boot/read nodes to that observed cause; it does not invent completed sweeps.
Dependent public flags become false only after actual server removal, complete
journal sealing, final catalog capture and verified owned-resource cleanup.

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
  exit code 0-255 and failure `null`, or `infrastructure-error` with a fixed
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
  their hashes bind canonical probe events captured by the owned collector.
- Migration: `{execution, log, markers, catalog_sha256, tables}`.

The top-level observations include the previous server boot/startup proof,
the sorted unique nonempty `expected_preexisting_tables` identity hashes from
the verified base catalog, before-forward table capture, target catalog, two forward passes, candidate
readiness/write sweep, stopped-candidate and zero-client quiescence with D0,
both N-1 migrations and the read-only control, all old-server sweeps/workers,
and the final catalog observation. They must come from one correctly ordered
experiment on the bound source and previous image. The evaluator cannot prove
collection completeness, provenance, ordering or signatures from booleans
and hashes alone; the owned runner measures and binds those facts. It must
never fabricate defaults when a phase did not run.
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

`runtime.py` supplies the bounded CLI foundation for the experiment. It drains
stdout and stderr concurrently with separate byte limits, imposes a deadline,
and reaps its child process. A completed nonzero exit is captured as evidence;
it is not automatically a compatibility verdict. Process or capture failures
raise fixed infrastructure errors without exposing arguments or stream data.
Optional standard input is limited to 1 MiB and written concurrently under the
same deadline; a blocked or incomplete write refuses the capture. Without an
input value, standard input remains disconnected.
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
container lifecycle, workflow or publication is added by this foundation.

## Synthetic API adapter

`sweep.py` performs the six API cases against a caller-owned synthetic stack.
It accepts only an explicit loopback origin or a Docker alias belonging to the
run's namespace, uses the existing cookie/CSRF and signed Graph-stub protocols,
and refuses redirects. The caller must establish network ownership, provision
the synthetic workspace/account and authorize its legacy reply path first.
The adapter does not provision accounts or retrieve credentials.

Each write uses generated synthetic values and retains validated record IDs in
memory for reads across server replacement. Readback checks the expected values
and workspace/account/conversation bindings for contacts, inbound and outbound
messages, a local draft template, an inert start-to-end flow, and chatbot
settings. Only the greeting message is patched; chatbot and AI automation must
remain disabled. Outbound evidence must match one successful Graph-stub send.
An explicit journal baseline may discard older retained history. Measured
operations require contiguous entries from the next sequence number, including
when the baseline cursor is zero; a baseline is not complete worker history.

Requests, response sizes and total time are bounded. POST/PUT operations are
never retried, and an ambiguous, unsafe or incomplete case prevents further use
of that adapter. Results expose six booleans only after complete readback. A
complete value mismatch remains false on later reads of the same written record
set; a later match cannot erase it. Missing evidence cannot become favorable
partial proof. Focused tests use fake HTTP
responses and verify payloads against current handlers. They do not establish
that two real server versions can complete these cases. Worker observation,
provisioning and container isolation remain runner responsibilities.

## Synthetic PostgreSQL collector

`postgres.py` collects table digests, DDL probe markers and complete log windows
inside an explicitly bound, task-owned PostgreSQL 17 container. It has no DSN
input. Container identity, label, image and network isolation are checked;
host mounts and published ports are refused. SQL reaches fixed database
`compat` through bounded standard input to `psql` inside that container.
The observer uses the fixed `/var/run/postgresql` Unix socket with password
prompting disabled. Only this disposable cluster uses local trust; application
TCP connections retain SCRAM authentication. No password is passed to the
collector and a failed socket connection has no TCP or password fallback.

The synthetic administrator observes all rows, including rows hidden by FORCE
RLS, while the migration owner remains a non-superuser and the separate runtime
role retains its restricted privileges. Observation never disables RLS.
One read-only repeatable-read snapshot inventories every public stored table,
partition and materialized view, including empty relations; foreign tables
refuse. Deterministic row encoding preserves nulls, duplicate rows and arbitrary
text. Forward comparison projects the original columns, while D0 and N-1
captures include all current columns. Missing or changed original columns,
incomplete output and exceeded bounds refuse.

A fixed administrator-owned probe outside `public` records DDL events. Its
markers are transactional and can roll back; they do not replace independent
statement logs and table digests. Phase boundaries use exact observer records,
zero remaining client sessions, unchanged container/probe bindings and an
append-only log prefix. Every byte between the boundaries is passed to the
existing strict parser. Foreign, unattributed, malformed or partial records
are not filtered away to make a phase pass.

Disposable PG17 validation covers actual inventory/row capture, FORCE RLS,
column projection, empty windows, a rolled-back UPDATE, DDL markers and the
fixed read-only `25006` control. The fixture was removed afterward. This is
collector validation, not a full release compatibility experiment. Catalog
normalization remains the existing Go command's responsibility; the collector
does not invent a second catalog hash or claim production-overlay coverage.

## Owned synthetic services

`stack.py` owns a disposable internal network and three services: the pinned
PostgreSQL 17 and Redis images, and the existing Graph stub built from the
bound source. Images must already exist; this module does not build or pull.
Graph image labels bind the expected source and recipe locally. They do not
replace separate build validation or image attestation.

Containers use tmpfs storage, a read-only root filesystem, explicit resource
limits and no published host ports or host mounts. The PostgreSQL observer's
fixed socket directory is pinned in the server configuration. Generated
fixture secrets remain private; public evidence contains only resource and
operation metadata. Process-running checks are not readiness checks.

Creation is attempted once. Cleanup uses recorded full IDs and freshly checks
ownership, configuration, network membership and start identity before acting.
An unknown command outcome or foreign network attachment refuses further
actions. Cleanup confirms each resource's absence and does not prune or adopt
resources by name. The mocked tests cover these boundaries and composition
with the socket collector. A disposable local probe passed three-service
readiness, host SCRAM refusal with working local socket, complete table capture
with FORCE RLS, and an empty phase window with unchanged public digests. The
final probe also completed the module's exact-ID cleanup and verified that all
12 original container IDs remained unchanged. Earlier refused attempts and
their separate exact-ID cleanup remain preserved as development evidence.
Cleanup uses the supported `--timeout` flag with strict ID acknowledgement.
Service creation now passes generated secrets through an explicit child
environment with key-only Docker arguments. A subsequent disposable fixture
run exercised that default transport and checked that generated secrets never
appeared in child arguments. Docker's private container configuration still
contains those environment values; it is not a public artifact. These checks
and cleanup do not constitute a release compatibility proof.

`stack.py` remains the three-service foundation. Explicit process registration
allows the separate lifecycle below to attach only its recorded owned IDs;
base cleanup refuses until those processes and their manager have closed.
The internal network gains no published host port or Docker socket.
Meta-relay integration remains outside this foundation.

## Owned application processes and private controller

`processes.py` adds fixed previous/candidate web, migration and ephemeral
controller lifecycles around a live `OwnedStack`. Cached image references,
image IDs, source commits and recipe hashes are required inputs. Previous web
images must use an exact release repository digest; candidate/controller local
image IDs must come from separately verified source builds. The module checks
image and container metadata but does not verify attestations, build or pull.
Released web images have no recipe label: that recipe binding remains the
caller's responsibility through the existing release/source verification.

Only one web alias can exist, and web and migration processes cannot overlap.
The fixed migration command uses `compat-fwd`, `compat-n1-rw` or
`compat-n1-ro`; the latter requests transaction read-only at connection start.
Database roles and the `compat` database must already be provisioned. Generated
synthetic configuration reaches an owned tmpfs file through bounded stdin,
never a host mount or secret argv/environment. Server configuration omits the
migration-owner URL. A returned web handle proves running/configured state,
not readiness or the startup contract. Migration exit codes remain raw
observations; callers must frame the SQL phase with the PostgreSQL collector.

`controller.py` accepts one bounded private JSON request on stdin. Its pure
parser runs before container creation and the owner checks the namespace.
Origins are exactly the owned web and Graph aliases. Readiness makes one
request to each service. Setup registers the synthetic Graph account and
checks its exact identity. Write/read operations reuse `sweep.py` and retain the
private record set for later cross-version readback; no fixture credentials
are retrieved or provisioned. The response preserves all six actual case
booleans, including false. Protocol success is not a compatibility verdict.
The controller has no Docker socket and is removed by exact ID after its
completed invocation. Invalid protocol evidence blocks subsequent starts or
sweeps while allowing known-resource cleanup. Unknown Docker outcomes retain
the stricter no-retry boundary and their resource evidence.

A disposable run of the signed previous web image passed startup and all six
API write/read cases, followed by retained-record readback. Its complete log
and journal observations passed, all owned resources were removed, and the
12 original local containers remained unchanged. This validates the previous
application path; it does not prove candidate migration or rollback.

`observation.py` checks the owned server's complete, append-only Docker logs
under a nonrotating `json-file` logging configuration. Startup requires the
actual RLS contract marker. The missing-marker FORCE RLS negative-control
fallback is not implemented, so missing evidence refuses the observation.
Worker observation requires at least 60 monotonic seconds and checks the
whole observed log period for panic and unknown-type lines. Graph journal
pagination accounts for the journal reads themselves; the final read's own
entry occurs after its returned snapshot and is explicitly outside that
snapshot. These observation paths have mocked validation, not an actual
60-second application-worker run.

## Owned synthetic fixture export

`fixture.py` provisions the fixed synthetic database and restricted roles,
then runs the test-only `TestCompatFixtureExport` in its own container sharing
the owned PostgreSQL container's network namespace. This keeps the exporter's
DSN on literal loopback without publishing a host port. The exporter uses the
reviewed production profile and source-owned global seeds, then creates a
synthetic organization, non-superuser account, entitlement and WhatsApp
account. Chatbot automation stays disabled; API sweeps create their own rows.

The fixture image is built from a pinned Go toolchain and a committed source
archive. Inputs use bounded private stdin; the adapter accepts one complete
result only after both the attached process and stopped container report exit
zero. It consumes the attempt even on failure. Cleanup removes only its exact
recorded container before releasing the services for their separate cleanup.

A disposable actual run passed fixture export with the observed production
catalog hash and private identity bindings. The exporter and all three
services were removed afterward, and all 12 pre-existing local containers
were preserved. This validates provisioning and default environment transport;
it does not establish application or migration compatibility.

## Ordered experiment and source-built images

`compat.py` composes the verified previous release, actual schema classifier,
production catalog contract, synthetic fixture and owned services. It runs the
previous API seed, two candidate migrations, candidate writes, quiescence and
D0, both previous-version migrations, the actual read-only control, previous
server reads and writes, the worker period and candidate reverse reads. The
journal is sealed and the final catalog captured before proof evaluation.
Public result bytes are returned only after exact owned-resource cleanup.

`images.py` admits four roles built during that invocation from one clean
selected commit: Graph stub, controller, fixture exporter and candidate web.
Each context is exported from Git objects and bound to fixed recipe hashes,
source manifests and inspected build results. Historical receipts and image
labels alone cannot admit an image. The previous web image is the exact
signed release digest; PostgreSQL and Redis also use fixed digests. Builds
use Linux/amd64, anonymous isolated Docker configuration and a pinned Buildx
binary. They do not publish images.

On Linux, admission creates and verifies one labelled, pinned BuildKit
container and volume, then binds the fixed Buildx driver to that existing
backend. Cleanup is confined to acknowledged and freshly verified owned
resources. An ambiguous outcome never authorizes name-based adoption or
broad removal. The Windows launcher contains the build client and its
descendants in a private Job Object; this does not claim that terminating a
client cancels all Docker daemon work.

One 2,400-second deadline covers release verification, pulls, all four builds,
the experiment and cleanup. The aggregate pull/build phase is capped at
1,500 seconds and must retain at least 720 seconds for the experiment plus
180 seconds for cleanup. A later phase never receives a fresh overall budget.
Cold-build timing and actual Linux backend composition still require real
validation. The public readiness switch remains false until the reviewed
validation sequence permits activation.

## Advisory workflow and release selection

`cli.py preview` freezes the latest and selected release identities from one
verified record chain and classifies latest-to-candidate. An explicit dispatch
rollback target selects a verified record without silently replacing the
latest-release preview base. `cli.py run` verifies the selected tag again and
requires its exact source SHA, then runs the experiment. There is no second
moving-latest selection.

The prepared `compat.yml` runs on every pull request, main push and explicit
dispatch. It has no path filter or first-attempt restriction. `release-preview`
feeds the exact same-run resolution into `compat-run`; the `compat` aggregator
requires both actual job outcomes and validates source, release, run and
attempt identity. Only a completely written, validated result receives the
upload marker. A measured negative result is retained as an artifact while
the compatibility check fails. Missing, malformed or incomplete evidence
cannot become an artifact by continuing after an error.

The final artifact name is `compat-result-<sha>-<run_attempt>`. The `attest`
job runs only after successful aggregation on push or dispatch, downloads the
same-run canonical result and revalidates it before signing. Pull requests
cannot sign results. Read jobs use contents/actions/attestations read access;
only the signing job has attestations and OIDC write permissions. No deployment
environment, provider credentials or production action is part of this
workflow. The existing CI release-tests job also discovers the compatibility
unit and workflow tests.

The experiment job is capped at 45 minutes. Checkout, artifact download and
tool setup share 180 seconds; the core uses 2,400 seconds, the client version
probe at most five seconds and result upload at most 60 seconds, leaving
55 seconds of margin. The step ceiling does not extend the core deadline.

## Validation still required

The completed local component checks cover the owned services, fixture export,
previous web startup and API sweeps, SQL collector formats and cleanup. The
first complete candidate/previous experiment and the actual hosted Linux
composition have not yet passed. The workflow is not yet published or enabled.

Completed retained-record read and write-readback mismatches now yield actual
per-case false flags. Writes still require complete acknowledgements, valid
record identities and all six readbacks with intact identity and safety checks.
The narrow postmigration panic path above has mocked coverage; it does not
establish a complete actual semantic-negative experiment or support arbitrary
terminal failures and blocked phases. A completed worker window of at least
60 seconds can preserve an observed panic or unknown-type failure while the
same bound server remains alive, its complete logs remain available, and the
actual stop, reverse sweep, final catalog and cleanup checks continue. Fatal
errors, worker stops and incomplete writes still refuse rather than substitute
invented completion evidence.

The continuous journal can attribute a narrow class of extra synthetic sends
to an explicitly bound old-server period. Each must match independently retained
fixture and nonce facts, have a fresh message identity, and receive both exact
status callbacks. Expected traffic must still be complete. After exact server
removal, the ledger reconciles the worker window against its full history and
counts each extra send once. Other unexpected traffic, ambiguous attribution,
missing callbacks, and mixed panic/unknown-type plus extra-send failures still
refuse. This path has mocked coverage; actual negative validation remains due.
Missing startup contract evidence also refuses: the FORCE RLS negative-control
fallback is not yet implemented.
These cases must never be represented by invented observations.

The current catalog adapter supports only the first released baseline
`e0016b3595114728e7864da2adb44c29065f25ef`. It requires the exact equal v0
source golden pins, no production profile or overlay at that previous commit,
and the candidate's exact reviewed production profile plus comments-only NOP
overlay. Other previous releases or catalog progression refuse. The fallback
uses the test exporter with a loopback-only DSN inside the owned PostgreSQL
namespace. Bootstrapping through a future previous image's
`rls-bootstrap-empty` command is not implemented. PR7 remains constrained by
the conservative migration closure independently of its dormant entrypoint
exemption.

The accepted production observation found no unknown public objects. Its
known-object hash is reproduced by the synthetic production bootstrap profile;
the accepted overlay contains comments only. The source golden remains
separate because column order and composite definitions differ. PostgreSQL17
validation alone neither establishes PostgreSQL14 equivalence nor authorizes
production schema changes.

Canonical acceptance still requires a green PR compatibility run, three
consecutive green main runs before PR16, and the actual swallowed-UPDATE
advisory case. Unit tests and the previous-application probe do not replace
those outcomes or any existing release protection.
