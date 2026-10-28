# Native aggregate identity inventory

This diagnostic is a private source proposal based on PR314
`a98dce1614be8e977f6ffdec61737c4e3892623f`. It is not installed in the accepted
production binary. Source review, exact-head hosted qualification, ordinary
release and a separately reviewed bounded invocation are prerequisites to use.
PR323's media inventory is a separate, unreleased mode; its payload snapshot and
private-file interface are not part of this command.

The proposed normal-binary invocation is:

```text
rereply catalog-snapshot -mode json -identity-inventory -organization-id <canonical nonzero UUID> -account-id <canonical nonzero UUID>
```

Only the existing optional `-config` flag is also accepted. Repeated flags,
positional arguments, other output modes, seed switches and arbitrary SQL, DSN,
role, time-window, row-limit or file-output flags are refused before loading the
configuration. Existing catalog and seed argument behavior remains unchanged.
The ordinary historical help text is unchanged; this document describes the new
mode. Runtime database configuration is used; `MigrationURL` is never a fallback.

## Session and scope

The command requires a native, non-superuser PostgreSQL17 database owner with
complete visibility of exactly seven ordinary public relations: organizations,
whatsapp_accounts, whatsapp_coexistence_states, whatsapp_identity_review_holds,
whatsapp_identity_review_members, inbound_events and channel_accounts. The global
organization relation must be unfiltered. Each of the six tenant relations must
pass the existing exact canonical FORCE-RLS owner/tenant policy and dependency
validator. Ownership, BYPASSRLS or an empty query result alone is insufficient.
No roles, grants, tenant settings, planner witness, migration lock or schema are
created or changed. The ordinary planner's admission remains unchanged.

One physical native connection owns two **sequential** repeatable-read read-only
transactions. Connect, BEGIN, admission, aggregate queries, validation, fresh
scope check and serialization share the existing caller-clipped15-second work
deadline. The original2-second cleanup reserve covers rollback, physical Close
and asynchronous CleanupDone. Statements are capped at3seconds, locks at1second
and idle transactions at5seconds. These are upper limits; earlier caller limits
still apply. No pool, nested transaction, callback or new work budget is used.

The first snapshot requires an exact live organization/account and exactly one
valid coexistence state. A private server-side SHA256 binds only bounded account,
state and shadow scope fields. No credential or sender field enters this witness;
the witness is neither exported nor an authorization token. After the first
transaction is rolled back, the same connection begins a new snapshot and repeats
native admission, visibility, physical-session and scope checks. Missing or
changed scope refuses all output. Hold additions after the initial snapshot are
not scope drift: this report describes that initial snapshot and does not prevent
later changes.

Shadow claims are selected by the expected legacy external key **or** the metadata
account claim, including soft-deleted rows. Zero claims is `absent`. Two claims
always refuse, including a live row plus a deleted predecessor. A single row must
have **both** exact selected-account selectors; a different, missing or malformed
selector refuses the whole report. Only then can the row be classified as
`live_exact`, `deleted_exact`, or `contradictory` for wrong channel/provider. This
last anomaly has unambiguous account scope and cannot qualify a receipt copy as
valid. No first-row selection or account-ambiguous attribution is performed.

Configuration loading and an arbitrary stdout writer are outside the database
budget. A separately reviewed process/transport deadline and output bound are
still mandatory for any live invocation. This proposal does not authorize one.

## Counts and interpretation

The report contains only fixed public labels, booleans and aggregate integers.
It emits no organization/account/contact/hold/event IDs, role/host/OID values,
sender identifiers or hashes, telephone numbers, message bodies, provider
payloads, headers, errors, credentials, arbitrary reasons or private files.
The invocation scope must be retained separately in authorized private custody.

The retained PR226 Query1 is embedded with only bound organization/account
predicates added: unsupported, open holds with stored `member_count > 0`, joined
to the exact state, grouped by organization/account/cycle/current-cycle, counted
with a separate sum of stored member counts. Public cycle rows use an ordered
relative ordinal and `current`, `older` or `future_invalid`; no numeric cycle or
scope identifier is exported. Zero Query1 rows is not absence of all holds.

Holds, member incidences and receipt rows have independent grains. A member or
receipt join cannot multiply hold counts. Stored member counts are compared with
actual selected member rows; invalid selector bits/nil IDs and duplicate member
incidences are reported separately. The diagnostic does not globally enumerate
foreign-tenant orphan rows. Receipts belong to the selected organization and
either a selected hold or the independently exact account-shadow binding. Copies
linked to another or absent hold, another account, a deleted row or invalid
protocol/signature/status remain fixed anomaly counts. Body/header/error columns
are never selected. Receipt counts are rows, not people or successful processing.

`recorded_verified_claim_shape` means only that stored protocol, provenance,
digest shapes, canonical selector bytes and generation/cycle fields meet the
closed projection. SQL uses the exact Unicode White_Space set used by Go
`strings.TrimSpace`, plus UTF8 **byte** limits. Invalid stored keys are unknown;
they are not trimmed or transformed into another sender for grouping. The
diagnostic does not reverify a signature, recompute a semantic/member digest,
prove current ownership, or establish routing/approval eligibility.

Within an account and cycle, recorded canonical nonempty direct identifiers are
deduplicated across generations. Without direct identity, canonical7–15ASCII
digit phones are separate candidate units. Parent-only, absent/unusable address
and invalid claim holds remain unknown hold incidences. A direct unit is never
merged with a phone unit or a different cycle/account. Multiple phone/parent
claims, shared-phone direct groups and phone-only overlap are explicitly
overlapping histograms, not proof of foreign ownership.97receipts are not97people.

`derived_reasons` retains unsupported-no-direct, unsupported-with-direct legacy,
supported-current, old-cycle-open, no stored member snapshot, member/selector
inconsistency and unknown recorded claim categories. These overlap and are
labelled `reason_source=derived_shape`; there is no stored `hold.reason` column.
`claim_provenance_unknown` includes structurally invalid protocol, selector,
generation or future-cycle fields, not only the provenance string. A missing or
inconsistent account/state causes whole-report refusal instead of a successful
reason count. Current decidability remains explicitly `not_observed`.

## Bounds and refusal

Exact cohort cap+1 reads precede aggregation:10000holds,100000member rows,
20000receipt rows and256cycle groups. Overflow, missing/ambiguous scope, policy
visibility failure, cancellation, query failure, inconsistent aggregate sums,
unknown/missing/null public fields, output overflow or incomplete disposal yields
one fixed failure and no partial counts. Operators cannot raise these limits.
Rows and aggregation still obey statement and whole deadlines; row limits are
not a time guarantee.

The public decoder has a512aggregate-row ceiling and64KiB byte ceiling. The
256cycle bound currently dominates the row ceiling: at most256cycle rows plus
four fixed projections exist. No database fixture claims that512successful rows
are reachable. The command includes its completion flags/newline in the final
64KiB check. There is no truncation, pagination, raw-selector buffer, seeding,
replay, worker claim, receipt recovery, mutation, API/provider call or grant.

## Prepared qualification

Eleven named groups cover mode isolation/configuration/privacy, native admission
and canonical visibility, retained Query1 and independent grains, recorded shape
and sender units, receipt/member anomalies, cap boundaries, repeatable snapshots
and actual second-BEGIN scope drift, causal lock cancellation/disposal, no writes
and closed bounded reports. Defensive malformed rows use explicitly generated
small projection tables. An additional control uses the existing full installed
bootstrap schema and seeded reseller-bound organization; impossible FK writes are
asserted as database refusals without disabling constraints or triggers.

The cancellation fixture requires the exact observer PID, exact held relation,
fixed visibility/cohort SQL and `pg_blocking_pids` linkage before cancel/timeout.
The collector's fresh-check fixture uses a test-only pgx query tracer to pause
its actual second BEGIN after rollback; it does not modify or emulate product
SQL. Owned result workers are canceled/unblocked and joined before normal fixture
cleanup. Remaining execution qualifications include all11groups without
environment skips, PG17/race, formatter/compiler/lint, existing seed/catalog/
planner regression groups, actual generated closure and registry/caps, ordinary
Compatibility and subsequent main/release qualification. No source hash or cap
repin is authorized by this proposal.
