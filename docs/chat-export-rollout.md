# Chat transcript export rollout

The reader, archive and UI require `chat:read` and the dedicated `chat:export`
permission. Native provider-neutral conversations additionally require
`conversations:read` and the active `omnichannel.enabled` product entitlement. A legacy WhatsApp message mirrored into the inbox is one
message, not a second export record. Assignment, active-transfer, organization,
account and current-conversation boundaries apply to every record.
Every endpoint requires the exact selected workspace header, with no fallback
to the user's default workspace after a switch or membership revocation.

## Permission gate

This source change deliberately does not add a row to `DefaultPermissions`,
change system-role grants, call a seed writer, or edit catalog hashes. The
capability is unavailable even to a superadministrator until the exact
`chat:export` vocabulary row exists. After the controlled vocabulary rollout,
ordinary role grants use the existing role editor; access is not admin-only.

The root-owned follow-up must review the single `chat:export` vocabulary
addition, source seed/golden differences, the appropriate versioned data step,
and rollback compatibility under the current Stage2 policy. Do not use runtime
bootstrap, a manual production insert, or `contacts:export` as a substitute.
There is no automatic manager/agent grant. Source preparation is not delivery:
an authorized non-admin must pass the complete export workflow after rollout.

The proposed future `DefaultPermissions()` entry is
`{Resource: ResourceChat, Action: ActionExport, Description: "Export chat conversations"}`
beside the existing chat read/write entries. This proposal does not authorize
running `SeedPermissionsAndRoles`: that writer reconciles the entire compiled
permission vocabulary, so this feature needs its narrowly reviewed data
operation. The operation must add only the approved vocabulary and preserve
existing grants, with any intended role grant handled explicitly afterward.

This patch adds no table, column, index or migration, so it proposes no change
to `DirectTenantTables`. Its four HTTP routes own read-only admission directly;
they must remain outside the ordinary `Tenant` wrapper. Root still needs to run
the required closure/classifier and route-ownership checks on the exact commit.
No generated registry, migration contract or catalog hash is asserted here.

## Reader and operational contract

Preview is time-stamped. Download takes a fresh PostgreSQL READ ONLY REPEATABLE
READ snapshot; records arriving after that snapshot are excluded. A dedicated
handler owns tenant binding and purpose admission instead of nesting inside
the ordinary transaction wrapper. It reads bounded keyset batches, never calls
the message-list handler, and never acknowledges messages or creates bridge
records. All matching pages are included. Selected scope uses server-issued
conversation keys, not the currently loaded contacts page.

ZIP construction finishes before any archive bytes are sent. A private temporary
archive and JSONL spool are removed on success, error or response close. There
are at most two builders and two retained archives per process, with one export
per user/organization across replicas. Preparation is limited to two minutes;
streaming expires after three minutes. The user lease and archive permit remain
held until response close, including client abort or expiry. The explicit limits
are 100,000 messages, 64 MiB of transcript/JSONL content and 64 MiB of final ZIP,
2,000 explicitly selected conversations, and a 4 MiB single text field. The
preview's conservative size estimate can require a smaller scope sooner. Oversized requests must use a smaller
date/account scope; no successful partial archive or silent truncation is
returned. Cancellation is owner-bound in Redis so another replica can receive
it. An atomic transition from preparation to delivery prevents a cancellation
accepted before delivery from returning a ZIP. After delivery starts, the API
truthfully reports preparation finished and the browser aborts its transfer.
Nothing is retained for asynchronous later download.

The snapshot transaction is released before a fresh uncached permission,
membership, account, assignment and history-boundary check of every exported
message. Loss of access fails the whole download. No second database connection
is acquired while the snapshot is held. A completed ZIP contains manifest.json,
messages.jsonl and readable UTF-8 conversation transcripts; attachment binaries,
object paths, signed URLs, raw metadata and provider credentials are excluded.
Delivery labels retain pending/failed/uncertain states and never assert a send
succeeded because an outgoing row exists.
Nullable legacy ingestion timestamps remain absent in JSONL with an explicit
ordering-fallback label; a provider timestamp is never invented as recorded
server ingestion time. Legacy bridge rows must agree on organization, contact,
channel, immutable source account ID, external ID and current account name.

## Required acceptance

Focused tests cover serialization, archive integrity, limits, request validation,
selection and keyset contracts. `TestChatExportPostgres*` and
`TestChatExportRedis*` use the existing disposable PostgreSQL/Redis fixtures;
they skip when their explicit test URLs are absent and start no containers.
They cover 45 contacts/225 messages, tied ingestion timestamps, concurrent
arrivals, assignment/active-transfer/session cutoffs, cross-org/account denial,
bridge deduplication and mismatches, unchanged unread state, fresh grant,
membership, assignment and entitlement revocation, exact workspace binding,
read-only tenant snapshots, cancellation and retained-stream bounds. They must
pass in scheduled CI before merge. Restricted runtime-role RLS and final
non-admin end-to-end rollout acceptance remain additional release gates. Frontend tests
cover stale/org/session changes, pagination, explicit failure and no-download
after cancellation. Root owns full integration, catalog/registry classification,
protected release and live fixture acceptance.

Run focused backend checks with `go test ./internal/handlers -run '^TestChatExportPure'`.
In the scheduled disposable PostgreSQL/Redis environment, run
`go test ./internal/handlers -run '^TestChatExport' -count=1` with the existing
test URLs. A skipped integration test does not satisfy acceptance.
