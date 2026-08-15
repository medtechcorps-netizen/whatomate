# Managed Instagram Login lifecycle

Managed Instagram Login is a default-off, one-workspace onboarding path for a
separate platform-owned Meta application. It does not import, rewrite, or
change the precedence of any existing static relay mapping. The existing
`/v1/meta/instagram/webhook` route remains static; managed traffic uses only
`/v1/meta/instagram/managed-webhook`.

## Trust and release boundaries

Production startup requires all of the following:

- `meta_instagram_onboarding.enabled=true`, the encrypted registry enabled,
  queue reader version 2, Redis, and an application encryption key;
- one canonical `allowed_organization_id`, never an empty or multi-workspace
  allowlist;
- an app ID, app secret, and verify token distinct from every Messenger or
  static Instagram credential;
- deployment-owned `app_review_status=approved` for intake, or explicit
  `quarantine_only=true` for an emergency downgrade; and
- the exact production bases `https://app.rereply.app` and
  `https://app.rereply.app/meta-relay`, the pinned Meta origins, bounded
  timeouts, and the managed relay route.

`app_review_status` is server configuration. It is never accepted from the
browser, onboarding request, tenant row, or an ordinary workspace
administrator. Production `quarantine_only=true` is the runtime kill switch:
OAuth start/callback, reconnect, subscribe, health approval, activation, and
all registry leases fail closed. Every API and worker process synchronously
reconciles the exact allowed workspace before opening its HTTP listener or
claiming work. One tenant transaction quarantines every managed-intent/residue
Instagram row, disables outbound and AI replies, and cancels all unsent
manual/AI outbox plus all cancellable scheduled AI work without a Graph call;
the process fails startup if that commit cannot complete. The lifecycle repeats
the same barrier before journal maintenance on every quarantine-only sweep, so
a cleanup failure cannot leave a row routable. A scheduled reply that already won the
serialized `generating` boundary may have disclosed its prompt to Qwen; its
result is rechecked and discarded after downgrade, and an ambiguous or stale generation
is terminally settled rather than replayed. Signed deauthorization and
data-deletion routes remain available, as do disconnect and recorded
unsubscribe reconciliation. A development tuple is invalid in quarantine-only
mode and impossible in production.

Development-mode App Review bootstrap is intentionally narrower than tenant
self-attestation. Only a nonproduction process may start unapproved, and only
with an exact deployment-configured tuple of organization UUID, app-scoped
OAuth subject ID (`/me.id`), professional Instagram profile ID (`/me.user_id`),
and Meta app role (`administrator`, `developer`, or `tester`). Production
startup rejects every development identity or role value. An approved release
also rejects leftover development overrides. Seeded legacy rows without exact
server-owned evidence are non-routable and quarantined.

## Onboarding and activation

A fresh OAuth registration is rejected if the organization already has any
static or managed Instagram relay account for that exact profile. ReReply does
not delete, rewrite, or silently adopt the old row. Operators must reconnect
the existing managed account, or explicitly migrate/remove the static account
through the established channel-management workflow before starting a fresh
registration. The organization mutex makes this conflict check atomic, so two
simultaneous callbacks cannot create duplicate accounts or subscriptions.

1. An administrator with both channel-account and integration write
   permission starts onboarding with an explicit `X-Organization-ID`.
2. ReReply creates a ten-minute, single-use Redis OAuth state bound to the
   exact tenant, initiating user, reconnect target, and a fingerprint of all
   managed app and registry settings. A separate high-entropy browser verifier
   is stored only as a server-side digest and sent in a Secure, HttpOnly,
   SameSite=Lax `__Host-` cookie; the reusable authorization URL never contains
   it.
3. The callback must consume both the exact OAuth state and the initiating
   browser verifier before any token or Graph call. It then re-authorizes the
   user and tenant, exchanges short- and long-lived credentials, introspects
   the token, and requires the OAuth/token subject to equal `/me.id`, the
   independently bound routing asset to equal `/me.user_id`, the exact platform app, professional
   account type (`BUSINESS` or `MEDIA_CREATOR`), and required Instagram
   Business scopes.
4. ReReply encrypts access and authority credentials. The relay receives only
   short purpose-bound leases and never receives the database encryption key.
5. A durable `subscribe` operation is recorded before provider mutation.
   ReReply then subscribes the exact profile and verifies the configured app
   has the `messages` field.
6. The account remains `pending` with outbound and AI replies disabled. Health
   must freshly verify the exact credential IDs and versions, token binding,
   profile, and subscription.
7. A second explicit `{ "approve": true }` action activates agent outbound.
   AI replies remain an independent opt-in.

The OAuth state's `IssuedAt` is persisted as a conservative lower bound for the
new authorization generation. Deauthorization and deletion journal first writes
take the same organization mutex as provisioning and rotation. Journal,
completed, or pending evidence from the same or a later second blocks credential
provision/rotation and subscribe; if OAuth won first, a later callback
quarantines or revokes that generation instead.

Every managed Graph request after token issuance includes `appsecret_proof`,
including long-lived exchange, introspection, profile reads, subscription reads
and mutations, health, refresh, revalidation, and disconnect. Provider bodies
and plaintext tokens are never written to logs or audit rows.

Registry leases are purpose-bound. Ingress, queued delivery, health, and
outbound requests cannot reuse one another's response. Health is the only
purpose available while pending. Worker and ingress leases require a verified
remote `messages` subscription; outbound additionally requires explicit
activation and `outbound_enabled=true`.

## Revalidation, refresh, and disconnect

The scheduler scans every managed Instagram row in the exact allowed
workspace, including legacy or downgraded rows that are not yet due. Runtime
configuration, release evidence, app/profile ownership, token scopes and
expiry, and the exact `messages` subscription are checked before renewing an
ownership lease.

Refresh replaces only the encrypted token material and timestamps on the same
OAuth credential ID and version. Reconnect creates a new credential generation
and transactionally cancels every non-dispatching unsent outbox job created
under the old generation. Disconnect, deauthorization, deletion, and
quarantine use the same cancellation fence. A job that already atomically
reached `dispatching` has won the provider-attempt race and is not rewritten;
pending, retrying, and processing jobs cannot later acquire newer authority. A
degraded binding may recover only to `pending`; it never auto-activates.

Scheduled AI generation has an earlier privacy boundary than Meta delivery.
Immediately before prompt construction and Qwen, the worker takes the same
organization mutex, locks the scheduled job/account/current credential pair,
revalidates exact release, profile, URL, callback, subscription, and credential
generation fences, then atomically marks the job `generating`. If a downgrade
commits first, Qwen receives nothing. If generation wins, any later drift
prevents AI message/outbox persistence. A Qwen error or lost `generating` lease
is terminal because retrying old customer text under a later OAuth generation
would cross the authorization boundary.

Disconnect first commits a non-routable pending operation, disables outbound
and AI, and cancels all non-dispatching unsent outbox and scheduled AI work.
Provider unsubscribe happens outside the
database transaction. Exact remote absence is then recorded and both
credential envelopes are revoked. Ambiguous outcomes stay quarantined and
require the explicit reconcile endpoint; reconnect cannot bypass them.

## Deauthorization and data deletion are separate

Meta deauthorization uses
`POST /api/integrations/meta/instagram/deauthorize`. Its signed request is
verified with only the managed Instagram app secret, journaled by digest, and
resolved only inside the deployment-configured tenant's RLS transaction. The
signed `user_id` is the app-scoped OAuth subject and must equal the persisted
authorizing-user ID; the external account and authority asset independently
remain the professional `/me.user_id` used for webhook and message routing. The
exact binding query is capped at two rows so ambiguity fails closed without
cross-tenant enumeration. Foreign rows cannot shadow, poison, or be mutated by
the one-workspace callback. The pre-existing Messenger resolver and SQL
signature remain unchanged and Messenger callers continue to use them.

Meta data deletion uses a different callback and status route:

- `POST /api/integrations/meta/instagram/data-deletion`
- `GET /api/integrations/meta/instagram/data-deletion/status/{confirmation_code}`

Data deletion has its own durable journal and creates an idempotent privacy
request with a 128-bit confirmation code. It never writes the deauthorization
journal. Exact retries return the same code and status URL. A delayed deletion
request creates the privacy workflow but cannot revoke a newer reconnect
generation. A same-second or missing generation fence quarantines the account
for reconciliation. An unresolved deletion fence is non-routable, cannot be
cleared by ordinary Graph health, and can be settled only by safe manual
reconciliation or an OAuth state, token inspection, and newly created
credential that are all strictly newer than the deletion event second.

An ambiguous same-second deauthorization is likewise a first-class routing and
activation fence. The lifecycle immediately revalidates it even when the prior
ownership check is fresh, using an internal exact app/profile/credential
snapshot that is unavailable to leases and product endpoints. Exact current
authorization evidence clears it only to `pending`/`awaiting_health`; a
strictly newer reconnect may preserve the new generation. A revoked result
revokes the exact fenced generation. Callback replay then settles the durable
journal without cross-matching Messenger credentials.

Both callbacks reject unsigned, query-only, wrong-content-type, or oversized
requests before tenant lookup. Instagram target lookup is an exact
configured-tenant RLS query with `LIMIT 2`; it has no global pager or
cross-tenant cardinality dependency. The deauthorization journal retains
completed entries for 30 days and unresolved entries for 90 days. The deletion
journal is tenant-owned from its first verified insert, including no-current-
account requests, and retains completed entries for 365 days and unresolved
entries for 90 days. Cleanup deletes at most 500 entries per state class per
sweep. Neither journal stores the raw signed request, access token, or app
secret.

## Reader-first rollout and rollback

Use this order:

1. Deploy the database migration and new reader binary with managed Instagram
   disabled. The deletion journal joins the direct tenant-RLS table set. No new
   Instagram `SECURITY DEFINER` resolver is installed or granted; the
   deployment-configured organization enters an ordinary tenant transaction,
   and routing version 5 remains unchanged.
2. Deploy queue-v2 readers to every relay replica. Confirm static webhook and
   outbound smoke tests still use the existing static mappings and routes.
3. Prove the intended pilot profile ID is absent from
   `META_RELAY_ACCOUNTS_JSON` on every relay replica before starting OAuth or
   subscribing. Static account lookup intentionally wins over the registry.
   Keep the existing four production mappings unchanged and choose a
   non-overlapping pilot; if a future pilot overlaps, stop and complete a
   separate static-to-managed migration first: block that profile's static
   intake, drain and settle its legacy inbound/outbound schema-0/1 jobs while
   their static `AccountKey` still resolves, remove the exact mapping from
   every replica, roll every replica, and verify absence before OAuth.
4. Configure the distinct managed Instagram app, server-owned release
   evidence, exact one-workspace gate, callback URLs, and relay credentials.
5. Enable the registry and lifecycle for one synthetic staging profile first.
   Subscribe, test health, approve explicitly, send one text, disconnect, and
   exercise deauthorization and data-deletion status.
6. Enable the intended production workspace only after the exact web and relay
   revisions are running everywhere.

The legacy Messenger SQL functions and routing version are unchanged. Managed
Instagram adds no cross-tenant SQL execution surface: registry, lifecycle,
callback, and deletion-journal access all start from the server-configured
tenant and remain under its RLS policy. Startup verifies the direct-tenant
table policy; tests also assert the staged, unreleased Instagram resolver
signatures are absent and non-executable. Old binaries ignore the additive
table but cannot fulfill its retained status URLs.

Before rollback after any managed binding exists:

1. block new OAuth starts/callbacks and managed webhook ingress at the edge,
   while leaving the signed data-deletion callback and status route available;
2. deploy `quarantine_only=true` with the same exact app and workspace binding;
   require every API and worker replica to pass the synchronous pre-serve/
   pre-claim database barrier. A replica must fail startup unless the exact
   tenant rows are atomically disabled and all non-dispatching unsent work is
   cancelled without a provider call; do not rely on a later lifecycle sweep;
3. use only disconnect or recorded unsubscribe reconciliation for provider
   teardown while the new binary and exact workspace gate remain present;
4. stop producers and drain or safely park every schema-2 dynamic queue job
   with a queue-v2 reader, preserving `dispatching` settlement, and allow every
   scheduled `generating` job to finalize or terminally settle without replay;
5. verify no active, pending, or degraded managed Instagram row is routable and
   no deletion/deauthorization journal entry remains unresolved; and
6. disable the registry and managed-Instagram configuration, then roll back
   workers or relay readers. Leave the additive tenant-RLS journal table and
   existing routing functions in place during the old-binary observation
   window. Do not add the profile to `META_RELAY_ACCOUNTS_JSON` until managed
   ingress is blocked, the provider subscription is reconciled, all managed
   queues are drained or parked, and the managed binding is non-routable; once
   re-added, the static mapping wins on every replica.

A full rollback of the web binary is safe only when there is no retained
Instagram deletion status obligation, or when a compatible responder remains
online for both the signed callback and every retained confirmation URL. An
old binary ignores the additive deletion table but cannot serve a route it
does not contain. Remove RLS only through the documented migration rollback
after all web and worker processes are stopped.

Do not roll an old worker back into a mixed fleet and do not delete queue keys,
credential rows, privacy requests, or journals to force rollback. Those actions
break generation fencing or Meta status obligations.
