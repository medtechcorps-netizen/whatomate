# Managed Meta self-service control plane

The encrypted Meta registry now supports two default-off dynamic onboarding
flows: Facebook Login for Business for managed Messenger, and Instagram Login
for managed Instagram Business/Creator accounts. Each has its own platform app,
OAuth identity model, subscription lifecycle, callbacks, release gates, and
recovery path. Threads remains a separate dedicated/BYO OAuth lifecycle and is
not provisioned through this shared Meta registry.

Existing static relay mappings are a legacy/off-pilot compatibility boundary.
Do not change their webhook routes, app secrets, Page tokens, or environment
mapping while rolling out a managed profile. A static mapping must never contain
the same Page/profile ID as a dynamic registry binding: static lookup has
precedence and would shadow the managed route. Drain its legacy queue work and
remove the exact mapping from every relay replica before managed OAuth. Static
tokens can have a different application identity from the legacy webhook
signer, so neither managed lifecycle may infer or reuse that identity.

## Deployment-owned trust boundaries

- ReReply owns one dedicated managed Messenger platform application. Clinics
  authorize Pages through that application and never enter an app secret.
- The ReReply web service performs Facebook Login for Business, validates the
  returned BISU/SYSTEM_USER token, and stores provider tokens only in encrypted,
  versioned OAuth credentials.
- The relay has a distinct managed Messenger app ID, app secret, verify token,
  and webhook route. Every registry binding carries the non-secret platform app
  ID; the relay rejects a binding that does not exactly match its configured ID.
- The relay has no database credential or application encryption key. It obtains
  a short signed lease from the private registry broker after outer service
  authentication and per-request replay protection.
- Tenant routing stays inside a tenant transaction/RLS boundary. The globally
  unique Page claim prevents two workspaces from subscribing the same Page.

Managed Instagram applies the same encrypted-registry principle with its own
app/profile identity split and bounded active/quarantined organization sets.
Zero-target or ambiguous signed deletion requests belong only to a distinct
platform compliance organization; exact targets remain tenant-owned. See
[Managed Instagram Login lifecycle](./meta-instagram-managed-lifecycle.md) for
its release, callback, migration, and rollback gates.

## Enablement gates

Keep both `[meta_registry].enabled` and
`[meta_messenger_onboarding].enabled` false until all gates below are proven:

1. Deploy queue reader schema 2 to every relay replica and confirm old workers
   are gone before allowing a dynamic producer.
2. Set `server.write_timeout` to at least 120 seconds. Browser provider actions
   use the same 120-second class of deadline; the provider phase itself is
   bounded to 90 seconds.
3. Configure the web encryption key, separate registry service and edge
   secrets, and an exact private registry resolve URL.
4. Configure a dedicated managed Messenger app at both services. Prove the app
   ID and secret cryptographically; do not infer them from a callback URL or
   reuse either legacy static secret.
5. Configure the Meta callback as
   `https://app.rereply.app/meta-relay/v1/meta/messenger/managed-webhook` and use
   the exact deployment value of `META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN`.
   The ingress removes `/meta-relay`, leaving the relay's internal
   `/v1/meta/messenger/managed-webhook` route.
6. Configure Meta's deauthorization callback as
   `https://app.rereply.app/api/integrations/meta/messenger/deauthorize`.
   Meta's data-deletion callback is a separate compliance endpoint and must not
   be treated as deauthorization.
7. During the pilot, set `allowed_organization_ids` to exactly one canonical
   organization UUID and keep `allow_all_organizations=false`. Do not put a
   clinic name, Page ID, token, or app credential in configuration or docs.

The allowlist is a runtime boundary, not only an enrollment list. Removing the
pilot UUID immediately prevents inbound, outbound, worker, and health leases;
the lifecycle sweep then quarantines the managed account, disables outbound and
AI, and makes no Graph call. `allow_all_organizations` is reserved for an
explicit external release after the Meta app is Live and required permissions
have Advanced Access.

## Messenger onboarding and activation

1. The browser captures the active organization and sends an explicit
   `X-Organization-ID` on status, start, callback, select, reconnect, reconcile,
   approve, and disconnect calls. OAuth state and selection sessions are bound
   to that organization and user.
2. Facebook Login for Business returns an authorization code. ReReply exchanges
   it server-side and requires a durable SYSTEM_USER/BISU token in production.
3. ReReply validates the exact minimum flow permissions plus the Business/Page
   discovery permission used by the implementation, exact Page ownership, and
   both Page tasks required for messaging and `subscribed_apps` management. The
   BISU token remains the authority credential; the system user's `accounts`
   edge supplies a distinct Page access token, which is independently bound to
   the exact Page before it is persisted as the connected credential or used
   for Messenger operations.
4. ReReply commits a globally unique pending Page claim, encrypted credential
   versions, and a subscription-operation fence before any Meta subscription
   mutation. A losing workspace makes zero Meta calls.
5. After Meta confirms the exact managed app is subscribed to `messages`, the
   account remains pending with outbound and AI disabled.
6. A fresh relay/Graph health check revalidates the Page token and exact
   `subscribed_apps` state. An administrator must then explicitly approve the
   exact credential versions before the account becomes active.

All managed Graph calls include `appsecret_proof`. Legacy static sends remain
unchanged because their Page tokens can be issued by a different app.

## Operation fences and recovery

Select, reconnect, disconnect, and reconciliation serialize on a durable
operation ID plus OAuth/webhook credential versions. Disconnect disables
outbound and AI before the provider call. A transport timeout or process crash
leaves the account non-routable and fenced; elapsed time alone never clears an
ambiguous provider mutation.

The managed account UI exposes a derived, non-secret recovery flag and a
`Reconcile Meta subscription` action. Reconciliation performs a fresh exact
`subscribed_apps` read, repeats only the committed desired operation, and
finalizes under the original generation fence. Browser APIs never expose raw
account metadata, operation IDs, provider IDs, tokens, or event digests.

## Revalidation and deauthorization

The lifecycle scheduler runs before the ownership maximum age. It validates
the authorizing token kind/app, required permissions, exact Page ownership and
tasks, Page token binding, and exact managed-app `messages` subscription. Stale
authority is quarantined and queued work is parked; revoked authority is
disconnected and dead-lettered under the existing version fences.

Meta deauthorization signed requests are HMAC-verified before the authenticated
digest rate gate. A bounded, non-secret global journal records the verified
event digest, app/user correlation, issued time, and processing state so partial
tenant failures and provider retries remain idempotent. Completed events are
retained for 30 days and unresolved events for 90 days, then removed in bounded
batches. The journal stores no raw signed request or token and is not serialized
to browser, audit, or log surfaces.

A delayed event cannot revoke a newer reconnect generation. Same-second events
are treated as ambiguous: ReReply immediately quarantines the account and
requires exact current-authority reconciliation. Target processing is bounded
and attempts every matching tenant before returning a retryable failure.

## Reader-first rollout and rollback

Use this order:

1. Apply the RLS-compatible migration while managed lifecycle remains off.
2. Deploy reader schema 2 everywhere and verify the exact new application SHA.
3. Configure the separate managed app and callbacks, then enable the private
   registry with the one-organization allowlist.
4. Onboard and activate one pilot Page only after health and explicit approval.

Before the first managed binding or schema-2 dynamic job exists, the default-off
migration remains compatible with rollback to the foundation binary. After the
first managed binding or job exists, rolling back to an old binary is
prohibited: it cannot honor lifecycle fences and can reopen manual Messenger
creation. For a later rollback, first disable managed intake, remove the runtime
allowlist, reconcile every ambiguous subscription, and drain or park every
registry-fenced job with a reader-v2 worker. Roll back only after proving no
managed binding or dynamic job remains usable by the old binary.
