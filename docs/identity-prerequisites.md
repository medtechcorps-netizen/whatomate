# Identity inventory prerequisites

The read-only diagnostic reports structural prerequisites for identity inventory
without granting inventory access or exposing customer data. Use the command
from the accepted production release before any app-console invocation:


```text
rereply catalog-snapshot -mode json -identity-prerequisites
```

Only `-config` is additionally accepted. Duplicate flags, scope selectors,
inventory/seed switches, arbitrary SQL/DSN/role arguments and other modes are
rejected before configuration loading. The command uses the existing runtime
database configuration; it never falls back to the migration connection.

It reports fixed profile booleans and the seven existing inventory visibility
guards. Those guards reuse the inventory's exact relation, canonical policy and
dependency validators. False means **not verified**, not an empty relation.
No account selectors, customer rows, counts, credentials, role names, identifiers,
driver errors or SQL definitions appear in the report.

The observer has one native physical connection and one repeatable-read read-only
snapshot. It uses the existing caller-clipped 15-second work and 2-second cleanup
budget, with statement/lock/idle bounds. Before/after physical-session witnesses
must match; output is published only after rollback, Close and CleanupDone.
An aborted SQL transaction or incomplete cleanup refuses the entire report.

A successful prerequisite report grants no inventory access and proves neither
selected-account scope nor cohort bounds, row availability, current routing or
approval eligibility. The original inventory gates remain unchanged. Even when
all reported guards pass, the original command must still establish its own
complete admission, scope, snapshot, limits and cleanup evidence.

Run the released diagnostic through the accepted app environment under a
separately reviewed, bounded console invocation. The production database's
current network rules permit only that app. Do not open network access, install
ad-hoc tools in production or weaken role/RLS guards to make a local probe work.
