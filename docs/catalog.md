# Catalog observations and production shape

The catalog snapshot records database structure for release comparisons. It
opens the configured runtime connection with startup read-only mode and captures
metadata in one bounded, read-only, repeatable-read transaction. It does not run
the migration coordinator.

## Known-identity equality

The released version 0 golden catalog defines the participating public
identities. Their normalized attributes, including explicit missing markers,
determine the catalog SHA-256. Participating kinds are relations, columns,
constraints, indexes, triggers, functions, policies, sequences, types and
extensions. SQL definitions and other free-form structural values are represented
by hashes. Relation ownership and grants use symbolic roles rather than live
role names.

Use the same golden-only identity set when comparing the first production
observation, O-1, with a synthetic database. Adding an overlay identity to that
comparison would change the question and invalidate the original equality
claim. Overlay-aware inventory is a separate check of the synthetic database.

The default command is:

```sh
./rereply catalog-snapshot -config config.toml -mode compare -seed-counts
```

Exit zero means that a complete capture was produced. Inspect `match` and the
known-object `differences` separately. A complete observation with differences
is valid input to the production-shape review.

## Count-only metadata

Unknown public identities are counted by kind. A dependent-object count may
name its parent only when that relation is already known. Non-public schema and
object names are omitted.

The count-only section covers non-public schemas, non-public objects, other
roles, default ACLs, OIDs, reloptions, statistics, tablespaces, other ACL entries
and other policy roles. These values are recorded separately from catalog
equality. Managed-service roles such as `doadmin`, provider ownership details and
default ACLs can differ between production and disposable PostgreSQL instances;
their names and provider-specific settings are not portable schema identities.
Two separate booleans report whether the migration owner owns the database and
the public schema.

Optional seed summaries use the committed global-table allowlist. The current
seed comparison reports the permission-key count and key hash. It does not
export key values, tenant counts or business records.

## Reviewing the production shape

O-1 must come from the accepted catalog-bearing production release. Retain its
release, image and golden bindings alongside the safe output. Hashes identify
differences; they do not supply SQL definitions. Obtain any necessary structure
from already reviewed source or a separately bounded structural observation.

Each known delta receives one disposition: `keep`,
`contract-cleanup-later`, or `fix-bootstrap`. A verifier-pinned delta is corrected
in bootstrap, rather than hidden by a production overlay.

Production-only object names require individual publication clearance. Objects
without that clearance remain in `accepted_unpublished` counts by kind.
`n_other_schemas` records the non-public schema count. The optional
`-list-unknown` output belongs in the owner's private review and must not be
copied wholesale into commits, CI logs or release artifacts.

The production-shape overlay is structural, idempotent DDL outside
`internal/database`. Its acceptance checks must establish all of the following:

- Bootstrap plus overlay matches O-1 under the original golden-only identity rule.
- Every recorded known delta has an explicit disposition.
- The synthetic inventory has no unknown objects when overlay identities are known.
- Applying the overlay twice leaves the same catalog.
- Read-only `rls-migrate` succeeds, and schema-change classification is empty.

Staging and compatibility fixture consumers must explicitly select the accepted
production profile and apply its overlay. This construction happens only in
synthetic databases. Existing production rows and non-public ledger contents
are outside this fixture input.

## Implementation status

The released catalog command's O-1 capture found 72 known differences: column
positions in eight tables and the corresponding eight composite-type hashes.
No unknown public objects were observed. `production-v0.json` records every
difference as `fix-bootstrap`, plus the observed non-public schema count of
seven; it does not copy those schemas into a fixture.

The test-only production profile precreates those eight empty tables in the
observed column order, using the current GORM models for types, defaults and
primary keys. The existing guarded bootstrap then installs constraints, indexes
and RLS normally. It refuses a nonempty schema before mutation. The original
empty bootstrap and version 0 golden remain unchanged.

On disposable PostgreSQL, the profile reproduces the observed known catalog
SHA under the original golden-only identity rule, including all eight composite
hashes. The production-only additive overlay is an explicit no-op: applying it
twice preserves the catalog and leaves unknown-object counts at zero. The real
compiled `rls-migrate` command succeeds with both owner and runtime startup
connections forced read-only, leaving catalog objects and source seed keys
unchanged.

Permission keys are a separate data diagnostic, excluded from the known catalog
SHA. O-1 reported 140 keys; the current source bootstrap has 141. Removing only
the source pair `booking.settings/delete` reproduces the observed 140-key hash.
This identifies a hash-compatible vocabulary difference, not its cause or any
user's assigned permissions. The profile retains source-defined seeds and does
not reconcile production data.

`TestCompatBootstrapExport` now selects `BootstrapProductionShapeEmptyForTest`
plus the accepted overlay. Its caller-supplied target remains restricted to an
empty, disposable loopback database owned by a non-superuser with a distinct
unprivileged runtime role. The actual exporter is tested for the observed
catalog fingerprint, zero unknown objects, retained target and unchanged
repeat-refusal behavior. The original `BootstrapEmptyForTest` remains the
default for golden-generation tests.

Staging's runtime integration is a separate dependency; it has not switched
profiles in this change. The compatibility exporter is available for the
future harness, but this evidence does not claim a full compatibility run.
