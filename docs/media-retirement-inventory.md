# Private media retirement inventory

This component is a private proposal awaiting integration and live acceptance.
`internal/mediaretirement` defines fixed PostgreSQL reference projections and
validates their private wire pages. It neither connects to a database nor reads
objects, changes references, deletes attachments, or closes a provider account.

The nine families are message envelopes, message parts, campaign previews, call
recordings, inbound media, IVR audio, organization calling audio, catalog artwork
and identity avatars. Projections retain soft-deleted rows, use a stable UUID
cursor, and inspect all accounts in the selected organization. Provider IDs stay
separate from URLs and object keys. Recordings and configuration/artwork/identity
rows are protected; all inbound events are protected, including reserved review
receipts. No content, caption, full settings, headers, full metadata, full event
payload or recovery encryption-key reference is projected.

A page holds at most 100 rows. SQL bounds each serialized row at 16 KiB and the
whole page at 1 MiB, allowing 4 KiB for the envelope. The caller supplies a
remaining private-output budget of at most 8 MiB. A server guard refusal returns
an empty rejected envelope; the decoder returns one fixed error and no partial
page. Exact multiples of 100 require an extra empty terminal page. These are
observation limits, not permission to silently truncate an inventory.

Before these SQL statements can be used, a native gateway must validate the
physical PostgreSQL 17 owner session, read-only repeatable-read transaction,
statement/lock/idle/total deadlines, fixed search path, table ownership and exact
RLS policies plus dependencies for every listed table. It must bind and recheck
the selected organization/account UUID, current legacy name, duplicate and
soft-deleted name claims, exact channel bridge and relevant generations. A
current account name alone cannot establish historical attribution. Unknown or
ambiguous references remain protected.

The connection-owning native gateway and private-manifest CLI are implemented.
The gateway locks the closed relation set before its first snapshot-bearing
query, checks native owner/RLS visibility, completes every family page in one
physical repeatable-read snapshot, then rolls back and waits for Close and
driver CleanupDone before returning the bounded manifest. Native PostgreSQL
fixtures use disposable databases and synthetic rows. Their execution is tracked
separately from parser tests; neither proves live owner/RLS visibility.
Selected JSON reference fields are type-checked
on the server before transfer, as well as validated by the client. Pages
from different snapshots must never be presented as one atomic snapshot.

`ScopeProjection` and `DecodeScope` add a bounded current-selector witness. The
query includes deleted same-name account claims, requires both channel bridge
selectors to agree, and refuses duplicate or foreign-organization claims. It
binds account renames even when the timestamp is unchanged and includes any
coexistence generation. Its fixtures use synthetic data; execution evidence is
recorded separately. The witness explicitly denies complete historical name
attribution. It is used inside the native owner/RLS gateway's physical snapshot.
A same-snapshot recheck cannot detect a later
concurrent change; mutation needs its own fresh transaction and guards.

Reseller branding has wider scope and needs a separate bounded overlap check.
Recovery artifacts belong to recovery services and require that service's
protection check; do not expose backup coordinates or content in this manifest.
Scheduled hydration work needs a fresh generation/reference recheck before any
mutation. This inventory must not bypass reserved-event integrity triggers.

Only after closing the database connection may the separate bounded metadata
reader inspect exact, classified Spaces keys. Metadata 404, access denial and
network failure remain distinct; none proves Tigris provenance or deletion
eligibility. Working Spaces objects and unresolved references are preserved.
Old Tigris recovery is canceled. Any eventual retirement must use reviewed exact
before-values and fresh concurrency checks. Provider account closure still
requires legitimate ownership authority and an account-wide retained-resource
check.

## Private command

Run `/app/media-reference-inventory -organization-id UUID -account-id UUID
-output /absolute/private-directory/manifest.json` in the Linux application
environment. Configure the intended owner in the normal database connection;
the command never substitutes `MigrationURL` when the runtime connection is
ineligible. PostgreSQL TLS and normal connection selection are preserved. Role
and options-based startup overrides are refused. The database work is bounded,
with separate time reserved for connection disposal.

Create the output directory beforehand, owned by the process user with mode0700.
The command follows no directory or destination symlinks, refuses an existing
destination, and creates a mode0600 temporary file inside that private directory.
Only a complete manifest returned after database disposal is written. A synced
temporary file is installed with atomic no-replace rename, followed by directory
sync. A competing destination is preserved. Failed captures leave no final file;
unpublished temporary files are cleaned up. A failure after atomic installation
reports that the complete file's durability is unconfirmed. No private references,
organization/account IDs, driver errors or configuration paths are printed.
Other operating systems refuse output admission before connecting to a database.

This command inventories references only. It does not probe objects, infer
historical attribution, authorize retirement, or close any provider account.

The inventory tool is a separate executable and package. It reuses the existing
database visibility validators through a catalog-only adapter, without adding
imports or initialization to the server or migration entry points. The release
image includes the tool, but the regular server entry point does not invoke it.
Migration caps, generated registry metadata, DDL and data-step revisions remain
unchanged. Existing deployment and schema admission policy remains in force.
