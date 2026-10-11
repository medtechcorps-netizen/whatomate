# Compatibility experiment and proof

`release/compat/proof.py` is a pure standard-library evidence evaluator. It has
no Docker, GitHub, filesystem, network or database operations. Its synthetic
tests exercise proof rules; they do **not** establish compatibility of a release.
The ordered experiment, source-built image admission and hosted entry points
are implemented and enabled in this source for hosted qualification. The
local positive experiment and swallowed-UPDATE fixture-branch advisory have
separate source-bound evidence described below. Hosted CI qualifies each
candidate; three accepted consecutive main compatibility runs remain required
before PR16. Component tests do not establish release compatibility.

## Result and failure contract

`evaluate_proof(observations)` returns exactly these public fields:

```text
sha, previous_release, previous_web_digest, classification, forward,
old_migrate_ok {rw, ro, negative_control},
old_server_ok {boot, read, write, reverse, workers},
reversible, catalog_sha256, run_id, run_attempt
```

`classification` contains only `{kind, dormant}`. Current classifier outputs
are `none/false`, `data/true` and source-bound pending `expand/true`; active
`expand/false` and `contract/false` are representable for their planned proof rules. This module
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
hosted validation for each candidate. The readiness switch is enabled; the
accepted local advisory does not replace hosted qualification.

## Advisory workflow and release selection

`cli.py preview` freezes the latest and selected release identities, retained
database origin and actual web-image source from one authenticated source
operation. Normal classification compares that database origin to the candidate;
after an image rollback, it need not equal the latest record's source. `cli.py run`
authenticates the sources again and requires exact agreement of the canonical
version-4 snapshot with the preview before image admission or an experiment.
An explicit rollback target selects the separate proof lane only on a validated
`workflow_dispatch` of `refs/heads/main`. Other events or refs refuse before
client construction. The source resolver independently binds the tooling source
to captured current `main`; supplying a target does not authorize a deployment.

Read-only source preparation for that experiment uses a distinct resolution.
It binds the producer to captured current `main`, keeps both actual signed web-image
sources separate from their release records and the retained database origin, and
retains the complete ordered target-through-latest span. The
source gate requires the same seven active artifacts across complete producer and
span source material, in addition to the existing five fixed v0 profile pins; it
therefore refuses seed or registry transitions. Only the existing exact released
source exception permits absent profile/overlay files at a legacy image endpoint.
This is source consistency data, not a measured database result or signed rollback
proof. Normal snapshot4/context3 and the standalone no-proof rollback refusal
remain unchanged; the operational qualified route additionally requires fresh
signed evidence and complete custody.

The private rollback producer consumes that distinct resolution and keeps three
image roles separate: the exact signed target runs as the previous application,
the exact signed live release runs as the candidate application, and current-main
tooling supplies the catalog collector, controller, graph and fixture. Its four
tooling builds retain their real source and build receipts. Target and live pulls
share the existing build and overall deadlines; equal digests can share a pull only
when their source and recipe bindings also agree.

The experiment retains the existing migration, read/write, worker, reverse and
cleanup phases. Catalog collection and terminal failure evidence bind to the
tooling source, independently of the live application source. The proof lane does
not change normal snapshot4/context3, release admission or retry custody.
Controlled tests of the path are not actual database or hosted qualification.

The public preview/run commands bind that experiment to a separate
version-1 snapshot with purpose `rollback-v0` and context version 4. They require
an explicit target on current-main workflow dispatch, re-resolve the full source
projection inside the original deadline, and compare the fresh snapshot before
image admission. The binding retains both endpoint identities and actual image
sources, the retained database origin, the full ordered span commitment and a
digest derived from raw workflow, source material and both endpoint recipes.
That compact digest does not replace complete source validation or custody.

The separately named rollback adapter verifies both subjects through the same
pinned verifier and file-ownership checks as normal evidence. Its pure parser
accepts only the closed context4 shape; normal parsers reject it, and contexts1-3
cannot enter the rollback parser. Complete negative measurements retain their
actual flags and final catalog hash. Parsing or signing them does not make them
successful. File limits remain 16 KiB for results and 4 KiB for context.

For this lane, `check` and `validate-evidence` use a separate closed validator and
create-only writer for the same two canonical artifact names. Both files must
round-trip exactly before the complete marker. A complete negative fails `check`
but remains eligible for the unchanged workflow's upload and independent signing;
infrastructure, partial-write, failed-upload and cancellation paths do not become
ready evidence. The workflow bytes, permissions and limits are unchanged.

Operational rollback across two or more non-none changes uses the dedicated
fresh source/collection/source gate in both plan and production. Before any
provider request, it serializes and replays complete format-7 rollback custody,
then rechecks the selected artifact's expiry at full UTC precision. Missing,
negative, stale, oversized or mismatched evidence refuses. Successful PUT and
reconcile both carry that custody to the record job. Zero/one-change requests do
not fetch this proof, and an all-v1 chain cannot invent a retained v2 live contract.
The source gate still requires the unchanged reviewed-v0 seven-artifact profile;
this is not support for arbitrary multi-DDL histories. The record preserves the
live database tuple, writes the selected image sources and replays original-time
qualification without fetching expired artifacts or substituting current main.
The normal snapshot4/context3 contract is unchanged; rollback context4 makes no
claim about unapplied pending plans. Local synthetic routing and verifier tests
do not establish exact-source hosted rollback qualification.

The normal resolution format is version 4. Both `latest` and `selected` contain the
verified release tag, signed manifest SHA-256, source SHA and web digest.
`database_base` separately binds the supported legacy source or retained v2
origin record and measured catalog; `selected_web_source_sha` identifies the
actual selected image source. Mandatory `pending` binds candidate and latest
record sources, active-three hashes and complete pending file inventories. The
producer derives it from freshly replayed raw source; the planned target never
replaces the measured active catalog. Older resolutions are rejected, not silently
upgraded. The manifest digest comes from the verified record bytes, not a GitHub
metadata identifier. A legacy source has no invented measured live catalog.

`compat.yml` runs on every pull request, main push and explicit
dispatch. It has no path filter or first-attempt restriction. `release-preview`
feeds the exact same-run resolution into `compat-run`; the `compat` aggregator
requires both actual job outcomes and validates source, release, run and
attempt identity. Only a completely written, re-read and validated evidence
pair receives the upload marker. A measured negative result is retained while
the compatibility check fails. Missing, malformed or incomplete evidence
cannot become an artifact by continuing after an error.

The final artifact name is `compat-result-<sha>-<run_attempt>` and contains
exactly `compat-result.json` and `compat-context.json`. The result schema and
verdict are unchanged. The current normal producer emits context version 3
with exactly these fields:

- `version`, `repository`, `workflow_path`, `workflow_sha`, `event`, `ref`,
  `sha`, `run_id`, `run_attempt` and `requested_rollback_target`;
- `latest` and `selected`, each with `release`, `manifest_sha256`, `sha` and
  `web_digest`;
- `preview`, with `kind`, `dormant` and `blocked`;
- `database_base`, with `mode`, `source_sha`, `origin` and `catalog`, and
  `selected_web_source_sha`, copied from the authenticated resolution;
- `pending`, the closed source binding from the authenticated resolution,
  including explicit observed empty inventories when no plan exists;
- `result_sha256`, covering the exact canonical result bytes, including LF.

Both files are sorted compact ASCII JSON with a final LF. The result remains
bounded to 16 KiB and the context to 4 KiB. Normal selection requires the
result classification to equal the frozen database-origin-to-candidate preview.
The current normal admission requires the source-bound context-v3 pair;
historical context-v1/v2 positives do not establish this newer contract. Their
actual false flags remain available for negative auditing. Exact old context-v2
record retries retain a separate closed historical replay path.
Active schema changes remain unsupported. The qualified rollback route is
separate from normal evidence and never accepts a normal forward receipt.

After successful upload, a separate guarded `evidence_ready` output permits
the `attest` job on push or dispatch, including a complete measured negative.
Cancellation, partial writes or failed uploads cannot set that marker.
`cli.py validate-evidence` revalidates the exact two files against the same-run
resolution and current job context without running an experiment or resolving
latest again. It returns success for complete negative evidence while `check`
still returns failure. The signing job attests the result and context as two
individually required subjects; the signed context binds the exact result
digest. A consumer must authenticate both subjects and their matching source,
workflow, run and attempt. Successful signing does not make a failed `compat`
job or the overall workflow successful. Pull requests cannot sign either file.
Read jobs use contents/actions/attestations read access;
only the signing job has attestations and OIDC write permissions. No deployment
environment, provider credentials or production action is part of this
workflow. The existing CI release-tests job also discovers the compatibility
unit and workflow tests.

The experiment job is capped at 45 minutes. Checkout, artifact download and
tool setup share 180 seconds; the core uses 2,400 seconds, the client version
probe at most five seconds and result upload at most 60 seconds, leaving
55 seconds of margin. The step ceiling does not extend the core deadline.
The separate signing job has a six-minute ceiling: checkout and two downloads
have one minute each, the two signing steps one minute each, leaving one minute
for local revalidation and final enforcement. Both signatures are required.

The required Test workflow uses a CI-only source predicate for the sole legacy
migration-closure endpoint refusal. Its narrow exception requires full valid
v2 history, class `none` with dormant false, an actually changed closure hash,
unchanged present active artifacts and complete unchanged pending inventory.
Legacy public checks and rollback keep their existing decisions. Required Go
tests independently validate generated closure bytes and reviewed caps.
The existing compatibility producer still runs its complete experiment; neither
source classification nor a plausible generated hash supplies measured proof.

Test and Compatibility are separate workflows. Before the planned owner step
makes `compat` required, the controlled merge gate must bind both successes to
the current PR head/base/test-merge and exact run/attempt. Actions metadata can
identify the PR head while result/context identify its synthetic merge; neither
may be substituted for the actual main commit after merging. Main requires its
own signed qualification. This change does not configure repository-wide
enforcement or alter compatibility workflow permissions or source identity.

The bounded PR16 implementation now connects this normal producer to Release
plan, staging and production admission and schema-v2 records. It authenticates
the complete mixed record history, retained database origin, actual image
sources and compatibility pair. The consumer vetoes later or overlapping
incomplete attempts or authenticated negative results; a fresh later
positive may supersede older infrastructure gaps. The original collection
window and full visible attempt history cross to the record job unchanged.

The source classifier, producer and consumer recognize complete immutable
all-expand dormant pending plans. Both forward passes, N-1 read-write/read-only
checks, the independent 25006 control, old-server sweeps, worker observation,
reverse sweep and cleanup still run. The active catalog, 127-table inventory
and empty modification allowlists remain unchanged. Pending gates are source
metadata, not assertions that a migration ran or a gate passed.
Release handoff format 6 preserves complete raw pending material, original drill
observation and full visible compatibility history, plus the complete original
owner-label observation when required by the class. Supported none/dormant
changes require that owner field to be `None` and remain exempt from restore
drills. Historical formats 1-5 keep their exact field sets for same-run record
retry: formats 1-4 use the closed context-v2 route; format 5 retains context-v3
pending/drill custody without an owner observation. Retry replays preserved UTC
policy observations without fresh label collection or a new drill clock.
Snapshot version 4, context version 3 and the result schema are unchanged.

Normal source admission conditionally collects owner-label facts for an active
contract inside both original source operations, before their final source
rechecks and under the same client/budget. It binds an exact-main post-merge
owner event and compares every before/after observation field except ordered
UTC capture time. Generic PR preview and rollback do not perform this collection.
Current active semantic proof remains unsupported: valid label facts and a
closed custody schema do not make an active contract pass compatibility.
No workflow permissions changed, hosted token read access is not qualified by
synthetic tests, and these observations do not authorize production DDL/data.
The complete pending fixture passed unsigned PR270 Compatibility
[run 38145640684, attempt 1](https://github.com/medtechcorps-netizen/whatomate/actions/runs/38145640684)
at merge source `345571b51fdc7b6e6b45cb0a7fffe7ecd7b55865`. The verified artifacts
bind the actual empty latest-release inventory to all three candidate pending
files and the 4,452-object expand target. The full comparison classified as
`data/dormant`; all measured compatibility flags passed and the active v0 catalog
hash stayed `1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7`.
The pending declarations remained unapplied. Signing was skipped as required
for this non-main-base PR; it supplies no main-release authority or active
DDL/data proof.

Active semantic expansion and contract proof, hosted permission qualification,
actual dedicated multi-span rollback
qualification and compatibility-outage bypass remain unfinished PR16 work. Visible API
pagination cannot establish absence of entirely deleted runs, so it does not
authorize an outage bypass. This partial implementation does not complete the
canonical plan or enable active DDL/data. The PR8 qualification prerequisite and
three distinct signed main runs are accepted; this new source/context contract
still requires its own signed main qualification and protected release validation.
See [the Release runbook](release.md) for the current operational boundary.

## Nullable-column observation v1: actual synthetic PG17 qualification

The preparatory v1 observer/explainer has been checked against a complete actual PG17 fixture for adding one nullable, no-default `pg_catalog.int8` column. The source-defined synthetic inventory changes only the new column and its table row type; existing objects, ACL/security observations and old columns remain equal. The actual positive test also verifies runtime DDL rejection, owner DDL success, unchanged old row values and NULL values in both existing rows. These are test assertions; the exported catalog fixture alone does not prove row preservation or statement history.

Saved validation covers 21 actual catalog test groups across the original run and a focused correction, 26 pure Go test roots plus two fuzz seed groups (78 passing test/subtest events), 12 Python cases and 10 registry groups with no-update flags. The generated closure is exactly 229 files. Both actual runs restored the original 12 containers, 12 networks and 540 volume names, and the 18,855-byte fixture remained unchanged. Initial refusals and the original failed negative are retained.

The corrected negative requires a stored non-null default and rejects it. A separate actual test shows that PostgreSQL17 normalizes `SET DEFAULT NULL::bigint` on this ordinary column to no stored default and no change in the complete observation. Endpoint evidence cannot establish whether that catalog no-op SQL was issued. The real application bootstrap remains unsupported by v1 because its default ACLs are nonempty. This qualification does not enable active DDL, classifier/producer admission, constant defaults, concurrent indexes or full old-binary compatibility.

## Nullable-column observation v2: application default ACLs

The separate v2 observer retains complete PostgreSQL17 default-ACL rows,
including normalized owner, namespace, object kind, grantor, grantee, privilege
and grant option. Explanation requires the fixed application contract at both
endpoints: runtime SELECT/USAGE for sequences and DELETE/INSERT/SELECT/UPDATE for
tables, granted by the migration owner in public. Unexpected complete rows may
be parsed diagnostically but cannot qualify. Physical ACL shape, raw role and
namespace evidence, bounded inventory and transaction consistency are checked;
v1 still requires no default ACLs.

Actual synthetic PostgreSQL17 validation passed 27 catalog test groups and 58
test/subtest events, with no skips. The separate positive export repeats one of
those groups. Pure Go passed 34 test roots plus two fuzz seed groups (160 events),
and the independent Python v1/v2 validators passed 25 cases. Source-contract
mutation checks and 10 no-update registry groups passed. The reviewed generated
migration closure is exactly 230 files; all 21 data-step rows, seed declarations
and the closure-cap manifest remain unchanged. An initial source-contract test
assumed the wrong transaction signature; that test-only correction and its
original failure remain recorded separately from these final passing runs.

The 3,537,014-byte actual fixture starts at the complete 4,432-object source
golden and ends at 4,433 objects. Only organizations.extra and the existing
organizations row-type definition change. Both expected default-ACL rows, the
other security observations, old columns and unrelated objects remain equal.
The positive test asserts runtime DDL rejection, owner DDL success, unchanged old
row values and NULL in the new column. Every phase retained stable source bytes;
cleanup verified the original 12 containers, 12 networks and 540 volume names.

This source-golden bootstrap differs from the production-v0 profile and does not
qualify that live database shape. Complete source/profile and admitted-image
binding, active classification, named migration-path admission, the apply engine,
old-binary compatibility and ordered multi-operation proof remain prerequisites.
V2 supplies no active DDL permission and adds no constant-default, concurrent-index
or other operation support.

## Preparatory nullable source/profile binding

The pure profile bridge binds the retained origin's release/manifest/source and
catalog coordinates, the candidate source/coordinates, and four complete source
documents before it accepts any observations. The closed descriptor fixes one
nullable int8 operation, its renderer, exact source roles/paths/lengths/hashes,
complete expected before/after catalogs and the v2 default-ACL contract. Catalog
files use the existing Go struct serialization; sorted descriptor JSON is a
different wire format. Logical catalog version, wire version, source-file hashes
and observed catalog hashes retain their separate meanings.

Explanation reparses all retained inputs, compares both complete observed
catalogs to the independently bound source expectations, then recomputes the
unchanged v2 graph/security explanation. Opaque frozen holders do not bypass
validation: canonical output recomputes from their retained source and observation
bytes. Every source/observation slot counts toward the original 8 MiB aggregate
before decoding, even when multiple slots contain the same bytes; individual
catalogs/observations remain limited to 4 MiB.

The 18 new profile tests and 25 v1/v2 semantic tests passed with no skips and
1,682 stable source pins. The positive derives both expected catalogs from the
preexisting source golden and a separately reviewed model/query blueprint before
opening the actual fixture. Its old row-type definition must match the existing
golden hash before the declared extra slot may generate the target. It preserves
4,431 unrelated objects and explains the actual 4,432-to-4,433-object transition.
The complete six input slots total 7,062,107 bytes; canonical output is 2,223 bytes.
A complete 9,273,887-byte source/observation combination refuses at the unchanged
aggregate bound. Other negatives cover source/role mismatches, coherent but wrong
expected catalogs, malformed graph/security evidence and forged holders.

The tests use controlled source identities. This bridge does not authenticate
Git, signatures, source history or images, and no runtime caller or classifier
uses it. A future concrete caller must read fixed immutable source paths under
the original budget and final checks. Newly authored candidate bytes cannot be
claimed to have existed at a legacy retained origin. The production-v0 shape
remains distinct from the tested application golden; neither logical history nor
active production admission follows from this pure explanation.

### Preparatory production-v0 graph baseline

The fixed `catalog-snapshot -mode nullable-v2-baseline` command uses a complete
expected catalog reconstructed from the reviewed source profile. The owning adapter
captures the full V2 observation through the admitted candidate image, requires
the production-v0 catalog and two exact default-ACL rows, and binds the result
to that image's source and recipe. The fixed command uses the exclusive catalog
slot and removes its process before returning accepted bytes. Any failure is
sticky; a second attempt on the same owner is refused.

Only this command's owned log capture permits 4 MiB of stdout. Ordinary captures
retain their limits. The command remains read-only and executes no nullable
column operation. Its returned metadata is a private baseline observation,
not a transition explanation or release qualification.

Local validation passed 42 catalog tests plus two fuzz seed groups, 12 CLI tests,
86 Python tests and repository lint. Ten registry groups passed after explicitly
recording the one newly reached source file: the migration closure contains
231 files and 3,158 declarations. Data-step, seed and active catalog manifests
remain unchanged. The controlled graph fixture is synthetic and keeps the
source-derived expectation independent. A subsequent local qualification at
`dc6ba90d650884c7ef40ad4ab4fbfd2d266203c6` built fresh source images and matched
all 4,432 production-v0 objects and both default-ACL rows. Runtime DDL was denied
with SQLSTATE 42501; final catalog/table checks and cleanup restored the original
Docker resources. Independent review accepted this as a baseline qualification,
not an active transition or complete old-server compatibility result. The first
refused build attempt is retained separately. This does not complete PR16 or
activate production migrations.

## Source classification for planned columns and indexes

The classifier now consumes fixed optional declarations for one nullable int8
column, one int8 column with a canonical constant default, and one non-unique
concurrent btree index over an existing ordinary int8 column. The latter two
declarations live under `internal/dbcatalog/source-classification/`; the original
nullable declaration remains under its fixed profile path. These interpret
source intent. They do not execute SQL or prove that an index was built
concurrently.

Every declaration binds complete raw before/head catalogs, immutable source
identities, catalog versions and independent definition preimages. A column
explains only its new column and changed row type; an index explains only its
new index row. Combined rules must describe the same raw comparison and explain
disjoint objects. All remaining changes retain normal security, contract,
history and data precedence. Existing columns, table permissions and unrelated
objects cannot be hidden by a parent-wide exception. Nullable and constant
declarations together are ambiguous and refuse. All raw input roles share the
existing 8 MiB allowance before decoding.

The constant grammar is a bounded canonical signed-int64 string and uses a
fixed quoted PostgreSQL bigint literal. An isolated PG17.11 template test passed
18 precommitted cases, including both nullability modes and range boundaries,
with unchanged original Docker resources after cleanup. The index grammar is
limited to one existing int8 key, ascending order, nulls last and the built-in
btree operator class. It excludes expressions, predicates, included columns and
uniqueness. Index-name checks cover tables, indexes, sequences and standalone
composite types without confusing ordinary scalar/array type namespaces.

The combined source regression run passed 243 cases: 53 column, 25 index and
165 v2 classifier/history cases. The earlier nullable change passed 268 distinct
cases including the legacy classifier; unchanged legacy definitions were also
independently checked during the combined review. These source rules retain
the current producer, apply-engine and active-profile holds. Full relation/RLS
bundles and versioned additive tenant profiles remain separate PR16 work.

## Validation still required

The complete local synthetic experiment passed on candidate
`d9e164812a08f9e642b72c80749cdc5498543f81` against signed Release 13
(`prod-20261010T234723Z-e0016b35`). It completed both forward migrations,
old migration read/write and read-only checks with the negative control,
cross-version API reads and writes, the worker window, reverse sweep and final
catalog checks. All owned containers and the owned network were removed; the
original container and network ID sets and volume-name hash matched the
baseline. Independent review accepted the source-bound result.
This is local evidence for that candidate, not hosted qualification or proof
for later source changes. Hosted CI must qualify each candidate's Linux
composition; enabling the entry points does not establish that outcome.

The local swallowed-UPDATE advisory passed on fixture-branch candidate
`534ce02ca4d1c4147bff2fae80a4e0cae3b4eb02`. Its synthetic psql actor returned
exit zero in both modes: the read-write UPDATE produced a modification log,
and the read-only UPDATE produced SQLSTATE 25006 despite the swallowed error.
The real CREATE negative control also produced 25006. The evaluator therefore
reported `rw:false`, `ro:false`, and `negative_control:true`, detecting both
violations despite successful actor exits. Catalog and table-content checks
remained unchanged. The owned exporter, process manager and stack closed, and
the original 12-container, 12-network and volume-name baseline was restored.
This accepted local advisory validates the detector with a synthetic psql
fixture; its actor is not the signed previous binary. It is distinct from the
`d9e164812a08f9e642b72c80749cdc5498543f81` positive experiment and supplies
neither a full compatibility proof nor a hosted artifact.

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

The catalog adapter supports the exact reviewed v0 artifacts. The historical
`e0016b3595114728e7864da2adb44c29065f25ef` baseline alone permits an absent
production profile and overlay. Later verified previous releases must contain
the same pinned golden, global-table list, history, production profile and
comments-only NOP overlay as the candidate. The public catalog evidence
distinguishes this equal-v0 mode from the historical empty-overlay mode. The
profile's `released_source` remains the original production observation's
provenance. Release and image signature verification remains the caller's
responsibility; source equality does not authenticate a release. Other catalog
versions, changed or missing artifacts and arbitrary overlays refuse. Focused
source-admission tests cover this progression; a later-release full experiment
has not yet validated it. The fallback still uses the test exporter with a
loopback-only DSN inside the owned PostgreSQL namespace. Bootstrapping through
a future previous image's
`rls-bootstrap-empty` command is not implemented. PR7 remains constrained by
the conservative migration closure independently of its dormant entrypoint
exemption.

The accepted production observation found no unknown public objects. Its
known-object hash is reproduced by the synthetic production bootstrap profile;
the accepted overlay contains comments only. The source golden remains
separate because column order and composite definitions differ. PostgreSQL17
validation alone neither establishes PostgreSQL14 equivalence nor authorizes
production schema changes.

The actual local swallowed-UPDATE advisory requirement is satisfied by the
source-bound result above. Canonical qualification still requires a green PR
compatibility run and three accepted consecutive main runs before PR16. Unit
tests and local probes do not replace those hosted outcomes or any existing
release protection.

### Preparatory all-head rollback history

The pure rollback history and request bridge retain closed raw run, job, check,
artifact and per-head workflow observations. They derive execution windows only
from that head's exact reviewed workflow bytes. Missing or unrecognized source
has no supersedable window. Qualification requires an exact current-H context4
positive matching the target, live image, retained database origin, complete span
and source-material binding. Foreign, wrong-binding or unknown evidence that
cannot be proved finished before the selected execution vetoes qualification.
Only another complete exact-H, same-binding positive may overlap.

Validation covers at most 500 attempts across all visible push/dispatch main
heads, 501 workflow observations including H, and 12 MiB of retained workflow,
envelope and paired raw bytes. It checks contiguous attempts, global identities
and complete repeated inventories. Fatal collection issues require incomplete
coverage and cannot qualify. Coverage remains `complete-visible`; whole-run
deletion detection is explicitly unavailable.

The concrete rollback collector uses the fixed repository and workflow
API, enumerates all visible main push/dispatch heads without head/status/date
filters, and reads each head's immutable workflow through the owned Git reader.
It retains every attempt's closed raw run/job/check/artifact envelope and verifies
both exact artifact subjects once through the fixed verifier. A second complete
inventory must match; drift or unavailable coverage cannot qualify. Unknown
workflow source never borrows the current producer's execution window.

The collector shares one already-open caller allowance across Git, API and
signature subprocesses, binds its returned observation to actual captured
transcripts, and closes that collection phase on return or refusal. It does not
itself authorize provider effects. The rollback preflight resolves
complete signed source before and after collection using three fresh, closed
phase budgets under one original outer allowance. It requires complete source
equality, exact history qualification and a final check of the selected artifact's
expiry. It preserves the original history observation time rather than treating
the final source read as a fresh Actions observation. The returned result is data,
not transferable operational permission. The protected plan and production
callers construct this result internally for spans of two or more non-none
records, repeat admission independently and require complete format-7 custody
before provider access. The standalone no-proof rollback API still refuses
such spans. Existing normal collection predicates are unchanged.

Format 7 retains all raw source and history inputs rather than a winning receipt
alone. The record job replays qualification at the original captured times and
requires the exact signed parent, target, producer and workflow execution.
It accepts a later plain chain read that omits only the original authenticated
producer enrichment; it never substitutes later main or fetches old artifacts.
Exact already-published record bytes remain the first idempotency path.
Formats 1-6 keep their existing promotion schemas and historical replay routes.
These code paths and controlled tests do not establish an actual multi-change
production history, exact-source hosted rollback qualification or deleted-run
coverage. Existing all-v1 production history remains all-v1.

The preflight and unchanged normal preflight passed 63 synthetic cases (22 new
rollback cases and 41 normal cases), including source and inventory drift,
foreign-head vetoes, expiry, shared deadlines and aggregate command/byte limits.
These checks do not establish hosted signatures, permissions, deleted-run
coverage, future freshness or operational rollback admission.

The concrete collector and shared verifier/GET seams passed 99 synthetic cases;
the existing pure history/request bridge passed 43 more. Controlled immutable
Git, API and signature transcripts exercise the actual transport paths, including
resolve/collect/fresh-resolve composition. These 142 checks do not establish
hosted permissions, actual signatures or operational rollback qualification.

Saved synthetic checks cover 396 unique cases: 76 initial passes plus three
repaired fixture cases, 163 normal-collection cases and 154 custody/workflow cases.
The initial three fixture failures are retained; there was no fresh all-green
79-case rerun. These tests do not establish hosted rollback qualification.
