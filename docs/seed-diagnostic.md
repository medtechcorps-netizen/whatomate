# Read-only seed diagnosis

`rereply catalog-snapshot -seed-diff -mode json` reports two compiled seed
predicates: missing permission keys and platform-reseller mismatch counts. It
uses the configured runtime database connection. There is no DSN argument,
migration-credential fallback, repair mode or planner admission.

The flag requires JSON mode and rejects `-seed-counts`, `-list-unknown` and other
output modes before loading configuration. Ordinary catalog-snapshot modes keep
their existing behavior.

Admission requires a native, non-superuser database owner (`current_user` equals
`session_user`), PostgreSQL 17 primary, UTF8, origin replication mode, and a
read-only repeatable-read transaction. The owner may have managed-service role
attributes, but all five fixed tables must still be ordinary persistent public
tables, owned and SELECT-visible to that role, without RLS, policies or
inheritance. A restricted runtime login that lacks these facts is refused. The
command does not change roles, grants, row-security settings, schema or data.

One budget covers at most 15 seconds of work and 2 seconds reserved for rollback,
physical connection closure and pgx asynchronous cleanup. Statement, lock and
idle-transaction timeouts are bounded. The same backend/database/login identity
and application marker are checked before and after observation. No partial
report is published after failure or incomplete disposal.

The report contains at most 141 compiled resource/action permission pairs,
including whether a missing active key conflicts with a deleted row. Unknown
database keys, descriptions, user/tenant identifiers, names, emails, roles,
credentials and connection details are excluded. Reseller output consists of
fixed aggregate counts. `clear`, `drift` and `conflict` are checked against the
unchanged canonical seed predicates in the same transaction. An absent platform
row is drift even when its membership counts are zero. Aggregate scans can grow
with table size; the command bounds their duration rather than claiming constant
cost.

Successful diagnosis does not prove the other seed steps, role grants (including
the separate booking.settings:delete grant residual), clean weeks, catalog
compatibility or migration readiness. It does not repair observed drift. The
planner's generic role rules and separate held migration-lock witness remain
unchanged. Stage2 notice, freeze and activation gates remain separate; this
command adds no step to the fixed PR23 sequence.

Source preparation must pass the unchanged generated migration/data-step closure
contracts, normal CI and compatibility before release. A closure refusal is a
review decision, never permission to raise a cap or regenerate unrelated
registry metadata. Prepared PG17 tests use isolated generated loopback fixtures;
test skips or image builds do not qualify those database checks. The command is
unavailable in an older immutable image until a normal reviewed release includes
it.
