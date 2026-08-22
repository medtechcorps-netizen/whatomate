# Platform compliance organization bootstrap

This command is the migration-owner control plane for managed Instagram data
deletion and managed Threads compliance. It is not a tenant API and it is not
run implicitly at application startup.

The two product markers are independent exact JSON booleans:

```json
{
  "meta_instagram_data_deletion_compliance_tenant": true,
  "threads_managed_compliance_tenant": true
}
```

Every such organization also has the independent exact purpose marker:

```json
{"platform_compliance_tenant": true}
```

Missing markers, string values such as `"true"`, numeric values, and `false`
are not equivalent to the boolean `true`. A reserved product marker on an
ordinary organization is invalid and admission fails closed.

## Atomic birth only

A committed ordinary organization can never be reclassified. The reviewed
creation operation accepts a canonical UUID that has never existed and creates
the complete purpose organization, selected product markers, and audit evidence
in one PostgreSQL transaction. There is no visible bare or half-classified
state.

The exact migration-owned `SECURITY DEFINER` creator performs a strict insert.
Its owner, source, search path, ACL, companion row/audit/truncate triggers, and
contract fingerprint are installed and verified by the normal tenant-RLS
migration. Direct purpose inserts, ordinary-to-purpose updates, ad-hoc settings
updates, and runtime execution of the creator are rejected.

Organization UUIDs are monotonic. Every organization insert first claims its
UUID in the private append-only `organization_identity_registry`; existing IDs
are backfilled while the migration holds an `ACCESS EXCLUSIVE` organizations
lock. The unique claim serializes concurrent births, survives ordinary hard
deletion, and cannot be updated, deleted, or truncated. Primary-key changes and
`TRUNCATE organizations` are rejected. Creation checks active rows,
soft-deleted rows, prior identity claims, and historical rows in every
authoritative organization-scoped table, so a UUID cannot later be reborn as a
purpose organization—even after an insert and delete in one concurrent
transaction.

## Preconditions

Before running the command:

1. Apply the normal schema/tenant-RLS migration from this release. Do not run
   the bootstrap against a database whose guard contract is missing, disabled,
   tampered, owned by the runtime role, or configured with unexpected ACL/RLS
   visibility.
2. Deploy every tenant reader, writer, API replica, worker, and global scanner
   on the purpose-aware release. The release must reject purpose organizations
   from authenticated tenant selection and skip them in ordinary global work.
3. Ensure `database.migration_url` connects directly as the migration/table
   owner. `current_user` and `session_user` must be the same owner. `SET ROLE`,
   superuser substitution, a runtime owner, or an inherited owner grant is not
   accepted.
4. Ensure exactly one undeleted active reseller has slug `platform-direct`.
5. Choose a canonical non-nil UUID absent from every active/quarantine clinic
   set and every release-owned clinic routing set. Never reuse a known clinic,
   deleted organization, or prior test UUID.
6. Choose a reviewed operator/change identifier of 8–96 safe characters. It is
   stored in sanitized audit evidence; do not put secrets in it.

The command pins `search_path` to `pg_catalog, public`, bounds connection and
command execution, applies lock and statement timeouts, and never logs a DSN or
secret.

## Trust boundary

This release treats the direct migration/table owner as a trusted deployment
authority. The database contract prevents runtime roles, tenant code, inherited
roles, PUBLIC grants, and ordinary operator mistakes from manufacturing a
purpose organization or evidence. It does not claim cryptographic resistance
to a malicious migration/table owner: that owner can ultimately alter tables,
drop triggers, or replace functions.

If migration-owner compromise is added to the threat model, do not use this
creator as-is. Provision a distinct externally managed `NOLOGIN` owner for the
creator, guards, identity registry, and evidence ledger, keep the migration
login outside that role, and re-review the ownership/rotation process before
the first purpose creation. This application migration intentionally does not
assume `CREATEROLE` and does not create that stronger boundary implicitly.

## Create a purpose organization

Always dry-run first:

```powershell
rereply bootstrap-platform-compliance `
  -config config.toml `
  -organization-id 11111111-1111-4111-8111-111111111111 `
  -operator-run-id CHANGE-20260821-001 `
  -create-purpose `
  -feature instagram
```

Review the exact UUID, product selection, configured clinic sets, migration
identity, and zero-footprint result. Then apply the identical operation:

```powershell
rereply bootstrap-platform-compliance `
  -config config.toml `
  -organization-id 11111111-1111-4111-8111-111111111111 `
  -operator-run-id CHANGE-20260821-001 `
  -create-purpose `
  -feature instagram `
  -apply
```

Repeat `-feature` to select both products: `-feature instagram -feature threads`.

Creation uses canonical name and slug values derived from the UUID and stores
only the purpose marker plus the selected product markers in settings. It does
not seed users, roles, subscriptions, widgets, integrations, contacts,
conversations, accounts, or provider bindings.

An exact retry with the same UUID, operator run ID, and feature set is
idempotent only when the canonical row and exactly one matching creation audit
already exist. A different run ID, feature set, row shape, or audit state fails
as an already-used identity; it is never normalized silently.

## Enable a product marker later

For an already-created purpose organization, dry-run and then apply only the
selected marker:

```powershell
rereply bootstrap-platform-compliance `
  -organization-id 11111111-1111-4111-8111-111111111111 `
  -operator-run-id CHANGE-20260821-002 `
  -feature threads `
  -apply
```

The transaction locks the purpose row, rechecks active platform ownership and
the authoritative zero-clinic-footprint invariant, merges only the requested
key, and appends audit evidence. Unrelated settings are never accepted as a
storage surface on a purpose row; the database guard makes the entire canonical
purpose organization row immutable except for reviewed feature transitions.

## Remove a product marker

Removal is also explicit and audited:

```powershell
rereply bootstrap-platform-compliance `
  -organization-id 11111111-1111-4111-8111-111111111111 `
  -operator-run-id CHANGE-20260821-003 `
  -remove-feature threads
```

Apply only after the dry-run succeeds. Removal fails while the current release
configuration still names the organization for that product. Instagram
removal additionally fails while a nonterminal deletion/privacy obligation
exists. Retained audit, privacy, and deletion evidence is not deleted by marker
removal and is not purgeable in this phase.

The purpose marker itself cannot be removed. A purpose organization cannot be
archived, reparented, renamed, rewritten, hard-deleted, or reused. There is no
terminal purge operation in this release.

## Audit evidence

Creation is audited by the migration-owned `AFTER INSERT` trigger in the same
transaction as the strict organization insert. Later feature changes append an
exact `platform_compliance` audit row in the same transaction as the marker
update. Evidence contains only the exact marker transition, bounded operator
run ID, database `current_user` and `session_user`, and exact control-plane
operation.

The row-shape guard rejects extra fields, duplicates, free-form JSON, runtime
forgery, updates, soft deletion, and hard deletion. No access token, app secret,
DSN, signed request, or raw subject identity belongs in this evidence.

## Database barriers

The authoritative organization-table registry is single-sourced with the
tenant-RLS registry. Every new organization-scoped model must be classified as
prohibited or receive an explicit reviewed row-shape exemption with a reason.
Startup and `ApplyTenantRLS` verify this invariant.

Prohibited tables reject inserts and moves into a purpose organization.
Reviewed Instagram fallback/privacy tables accept only their exact documented
shapes and transitions while the Instagram marker is enabled. Privacy events
are insert-only; retained identity fields cannot be rewritten or soft-deleted;
deletion-journal completion must link to the admitted same-organization privacy
request. Ordinary cleanup skips purpose-owned evidence and continues processing
ordinary organizations.

Historical scans fail closed unless the migration role has effective SELECT
visibility through the exact migration policy on every FORCE-RLS table. PUBLIC,
group-role, restrictive, false, mistargeted, unexpected, or inherited policy
drift is rejected. Pre-tenant exemption tables must not unexpectedly use RLS.

## Rollout and rollback

Merge and deploy this additive guard/migration contract before the first
creation operation. If this work is combined with another tenant-RLS change,
resolve the shared registry and migration policy first, run the complete RLS
suite, then run the creator tests; never install only the CLI portion.

After any purpose organization is created, a pre-purpose/auth-guard binary is
not a valid rollback target. Keep all tenant admission paths and global scanners
on a compatible purpose-aware release. `RemoveTenantRLS` refuses to tear down
the additive guards while a purpose organization exists, and the purpose row
cannot be purged to make rollback appear safe.

Application product configuration may be rolled back only after its marker
removal gates pass and a compatible responder remains available for every
retained callback/status obligation. Do not delete audit/privacy rows, change
reserved JSON directly, disable triggers, edit RLS policies, or recreate an
organization under another path to force a rollback.

## Failure handling

Dry-run is read-only: it takes no row lock, writes no audit, and creates no
organization. Apply is a single PostgreSQL transaction. If creator validation,
the strict insert, the creation audit trigger, or commit fails, no purpose row
or audit evidence commits. There is no Redis or external serving-fleet state to
recover.

On any failure, retain the exact error and reviewed run ID, verify the normal
guard contract and current database identity, and retry only the same reviewed
operation after correcting the root cause. Do not use ad-hoc SQL or manually
weaken a guard to salvage a UUID.
