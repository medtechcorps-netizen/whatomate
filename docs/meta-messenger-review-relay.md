# Staging-only Messenger App Review relay

> **Not production-ready:** this runtime exists only to demonstrate one
> Messenger Page during Meta App Review. It is not a production transport, a
> general tenant connection path, an Instagram integration, or evidence that
> the Meta app has been approved. The relay remains inbound-only. ReReply may
> expose one separately fenced, human-initiated, text-only review reply while
> the customer-service window is open; this does not enable ordinary outbound
> delivery. Never deploy this mode to a production environment.

The review relay is an intentionally narrow companion to
[Managed Messenger onboarding](meta-messenger-onboarding.md). It lets one
deployment-pinned staging workspace receive Messenger text events while the
Messenger app is truthfully in Development mode or Live/Published mode and its
App Review status is truthfully `not_submitted`, `pending`, or `in_review`.
Live/Published mode permits the app-role review transport; it is not evidence
of App Review approval, Advanced Access, or production readiness.

The exact runtime mode is:

```text
staging_messenger_review
```

The corresponding server-owned ChannelAccount marker is
`staging_messenger_review_v1`. Neither value is tenant-selectable.

## Scope and invariants

One review deployment pins this complete authority tuple:

| Field | Required form |
| --- | --- |
| ReReply organization | Canonical non-zero UUID |
| Page-owning Meta Business | Canonical numeric ID |
| Messenger Page | Canonical numeric ID |
| Messenger app | Canonical numeric app ID |
| ReReply ChannelAccount | Pre-generated canonical non-zero UUID |
| Review generation | New canonical non-zero UUID |
| Authority expiry | Future canonical UTC RFC3339 timestamp ending in `Z` |

Every tuple field must match in ReReply, the review relay, the managed account,
and the current webhook credential. A mismatch, expired authority, changed
credential generation, unavailable broker, or invalid proof fails closed.

The Page-owning Meta Business in this tuple is not necessarily the Tech
Provider Business that owns the app. Configure the app owner separately and do
not substitute one Business ID for the other.

The review account remains deliberately unconnected:

- status stays `pending` and `connected_at` stays empty;
- `outbound_enabled`, `ai_reply_enabled`, and default-outgoing remain false;
- the successful onboarding state is `review_relay_ready`;
- it is never recognized as a production protected-registry account; and
- generic update, Test, delete, send, mark-read, media-fetch, subscribe,
  credential-refresh, outbox, and AI-reply paths reject the binding.

The only outbound exception is the dedicated App Review reply endpoint
described below. It does not change any of these account fields, cannot use the
generic composer or worker, and never grants the relay an outbound credential.

## Security boundary

The public browser completes the normal authenticated FLfB onboarding flow,
but it never receives a Page token, webhook HMAC, broker key, or provider-proof
key. Credential provisioning is server-to-server only:

1. The relay signs the exact request body to
   `POST /api/internal/meta/messenger/review/provision` with a timestamp and a
   random one-time nonce.
2. ReReply authenticates the request before accessing Redis or tenant storage,
   rejects requests outside the one-minute window, and consumes the nonce with
   Redis replay protection.
3. ReReply locks and verifies the exact pending account and current
   credential. Its broker query does not select the Page OAuth token or an
   outbound HMAC.
4. ReReply seals only the current inbound webhook HMAC, credential ID and
   version, exact callback URL, and authority tuple in an authenticated
   encrypted bundle. The issued bundle normally expires after 30 seconds.
5. The relay decrypts the bundle in memory. Ingress may cache it briefly
   (`2s` by default and never more than `10s`); the queue worker re-resolves the
   binding without a cache before each forwarding attempt.
6. The relay signs the canonical inbound body with both the account inbound
   HMAC and the deployment-held provider proof. It also binds the review
   generation, credential ID, and credential version into a separate review
   proof.
7. ReReply re-locks and revalidates the account, credential, generation, and
   proofs before raw-event persistence and again before normalized-event
   persistence. This fences webhook delivery against concurrent
   deprovisioning.

The Page OAuth token never leaves ReReply, and the review credential row does
not create an outbound HMAC at all. The review relay has no Page-token setting,
no outbound HMAC, no static account inventory, and no outbound HTTP route. In
review mode it exposes only:

| Route | Purpose |
| --- | --- |
| `GET /livez` | Process liveness |
| `GET /readyz` | Bootstrap-safe Redis and provider-proof service readiness |
| `GET /reviewz` | Fresh, exact broker-binding readiness after onboarding |
| `GET /v1/meta/messenger/webhook` | Meta callback verification |
| `POST /v1/meta/messenger/webhook` | Inbound Messenger webhook |

Instagram webhook routes, account-health `HEAD`, and relay outbound `POST`
routes are not registered. A foreign Page ID in an otherwise valid Meta
payload is rejected before it enters the queue.

## Reviewer-only manual reply

Meta App Review may require a reviewer to demonstrate an app-to-user
Messenger response. ReReply provides a separate, staging-only exception for
that evidence without weakening the inbound relay or the normal outbound
fuses:

```text
GET  /api/conversations/{conversation_id}/meta-review-reply
POST /api/conversations/{conversation_id}/meta-review-reply
```

The GET endpoint returns a short-lived server attestation only when all of the
following remain true:

- the authenticated principal is the exact deployment-pinned reviewer user
  with the exact deployment-pinned reviewer role; that role's complete
  permission set is exactly `channel_accounts:read`, `contacts:read`, and
  `conversations:read`, with no write or administrative permissions;
- authentication is an interactive user session, not an API key;
- the deployment, organization, Business, Page, app, account, generation, and
  authority expiry match exactly;
- the managed account remains `pending`, review-ready, subscribed, not
  connected, not default-outgoing, and has outbound and AI explicitly false;
- exactly one current encrypted Page OAuth credential matches the configured
  app, Business, Page, account, and generation;
- the conversation belongs to that exact account and its customer PSID comes
  from the server-side contact identity, never from the browser;
- a verified inbound customer text exists and the Meta 24-hour
  customer-service window is still open; and
- service-purpose consent remains allowed.

The POST endpoint accepts only the issued attestation, a canonical client
request nonce, one nonblank plain-text body of at most 2,000 characters, and
an explicit `manual_confirmation=true`. The durable one-attempt key, Page ID,
recipient PSID, purpose, credential, and Graph route are server-derived. It cannot send media,
templates, tags, reactions, read receipts, AI output, automated replies, or a
business-initiated message outside the service window.

Before Graph is called, ReReply locks and revalidates the exact account,
conversation, consent, OAuth credential ID/version/ciphertext, generation, and
attestation. It then writes a durable single-attempt dispatch record. A
matching replay returns the stored result; a different body for the same
verified inbound message is rejected even when the browser supplies a new
nonce. Transport ambiguity, timeout, an invalid response, or an
unsettled provider result is terminal and is never retried automatically.
Only the safe provider message identifier and settlement state are retained;
provider response bodies and credentials are not logged or returned.

One absolute delivery deadline is established before the durable dispatch
begins and remains in force through locked revalidation, credential decryption,
and the Graph request. It is the earliest of the short transport cap,
attestation expiry, 24-hour service-window end, review-authority expiry, and
persisted provider deadline. If it expires at any point before transport, the
attempt settles locally and no Graph request is started.

Deprovisioning takes the same account lock. If it wins first, a review reply
makes no provider request. If a reply has already crossed the durable dispatch
fence, deprovisioning returns a retryable draining response and keeps the
credential until that one attempt settles. Generic outbox workers and relay
outbound routes remain unable to deliver the review message.

The review dispatch fence is permanent, not lease-expiring. A crashed or
otherwise unsettled `review_dispatching` record must never be cleared merely
because time passed: it blocks credential revocation and deprovisioning until
the provider outcome is deterministically settled or a separately authorized,
audited operator reconciliation records the outcome. Generic worker discovery,
claim, and stale-recovery paths exclude both the dedicated status and the
review idempotency namespace.

`/reviewz` is registered only in the staging review runtime. It bypasses the
short ingress cache and returns a generic `503` unless Redis, broker
authentication, the exact tuple, current credential, callback, expiry, and
independently derived key fingerprints for both the Messenger App Secret and
provider-proof secret all validate. `/readyz` deliberately remains service-level
so the first relay deployment can become healthy before onboarding creates the
pinned account; never use `/readyz` as proof that the review binding exists.

Broker and callback HTTP clients bypass environment proxies and re-resolve on
every new connection. The whole DNS answer is rejected if any address is
loopback, private, link-local, carrier-grade NAT, multicast, unspecified, or a
reserved/documentation range. TLS certificate and hostname verification remain
enabled, and redirects are disabled.

## ReReply staging configuration

First configure the ordinary managed Messenger onboarding values described in
[meta-messenger-onboarding.md](meta-messenger-onboarding.md):

```text
WHATOMATE_APP__ENVIRONMENT=staging
WHATOMATE_APP__ENCRYPTION_KEY=<existing ReReply encryption secret>
WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED=true
WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID=<numeric Messenger app ID>
WHATOMATE_META_MESSENGER_ONBOARDING__CONFIG_ID=<numeric FLfB configuration ID>
WHATOMATE_META_MESSENGER_ONBOARDING__OWNER_BUSINESS_ID=<Tech Provider Business ID>
WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET=<secret>
WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_API_VERSION=<explicit version, for example v25.0>
WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_BASE_URL=https://graph.facebook.com
WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL=<canonical public review relay base URL>
```

Then configure the exact review grant:

```text
WHATOMATE_META_MESSENGER_REVIEW_RELAY__ENABLED=true
WHATOMATE_META_MESSENGER_REVIEW_RELAY__MODE=staging_messenger_review
WHATOMATE_META_MESSENGER_REVIEW_RELAY__ORGANIZATION_ID=<organization UUID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__META_BUSINESS_ID=<Page-owning Business ID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__PAGE_ID=<Messenger Page ID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__CHANNEL_ACCOUNT_ID=<pre-generated account UUID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__GENERATION=<new review-generation UUID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__EXPIRES_AT=<future canonical UTC RFC3339 timestamp>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__REVIEWER_OUTBOUND_ENABLED=false
WHATOMATE_META_MESSENGER_REVIEW_RELAY__REVIEWER_USER_ID=<dedicated reviewer user UUID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__REVIEWER_ROLE_ID=<least-privilege reviewer role UUID>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__RELAY_BASE_URL=<same canonical relay base URL>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__REREPLY_BASE_URL=<public ReReply staging origin>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_AUTH_SECRET=<secret>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_WRAP_SECRET=<secret>
WHATOMATE_META_MESSENGER_REVIEW_RELAY__PROVIDER_PROOF_SECRET=<secret>
```

`REREPLY_BASE_URL` must be an exact public HTTPS origin with no path, query,
fragment, or credentials. `RELAY_BASE_URL` must be the exact canonical public
HTTPS prefix also used by `TRUSTED_RELAY_BASE_URL`; do not add or remove a
trailing slash on one copy.

The application encryption key, Meta app secret, broker-auth secret,
broker-wrap secret, and provider-proof secret must each contain at least 32
bytes without whitespace and must all be distinct. Store every secret as an
opaque secret-manager value. Do not add it to a deployment spec as inspectable
configuration, place it in a browser variable, copy it into tenant JSON, log
it, or paste it into this runbook.

The normal production relay trust settings must be absent in this mode:

```text
WHATOMATE_META_RELAY__BASE_URL
WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON
WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET
```

Review mode and the normal protected production relay configuration cannot be
enabled in the same ReReply process.

### Mandatory two-deployment reviewer-outbound rollout

Never enable reviewer outbound in the first rolling deployment that introduces
the dispatch/deprovision fences. An older web instance does not understand the
new permanent review-dispatch state and could otherwise deprovision the Page
while a new instance is sending.

1. Deploy the fence-aware build with
   `REVIEWER_OUTBOUND_ENABLED=false`. The reviewer user and role pins may remain
   absent in this first deployment.
2. Wait for the deployment to become fully `ACTIVE`. Verify the exact expected
   source commit for every web, worker, relay, and migration component; verify
   all previous web and worker instances have terminated; then verify normal
   health and review readiness.
3. In a second deployment, set the exact reviewer user and role UUID pins and
   change `REVIEWER_OUTBOUND_ENABLED=true`.
4. Do not roll back to a fence-unaware build while a review send is
   `review_dispatching` or its outcome is unresolved. First settle or reconcile
   the attempt through the audited operator procedure.

## Review relay staging configuration

Configure the separate `cmd/meta-relay` service with the same tuple and shared
secret values:

```text
META_RELAY_RUNTIME_MODE=staging_messenger_review
META_RELAY_DEPLOYMENT_ENVIRONMENT=staging
META_RELAY_REDIS_URL=<Redis URL>
META_RELAY_REREPLY_BASE_URL=<same public ReReply staging origin>
META_RELAY_REREPLY_PROVIDER_PROOF_SECRET=<same provider-proof secret>

META_RELAY_MESSENGER_APP_SECRET=<Messenger app secret>
META_RELAY_MESSENGER_APP_ID=<same numeric Messenger app ID>
META_RELAY_MESSENGER_APP_MODE=<development|live>
META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID=<Tech Provider Business ID>
META_RELAY_MESSENGER_TECH_PROVIDER_STATUS=verified
META_RELAY_MESSENGER_APP_REVIEW_STATUS=<not_submitted|pending|in_review>
META_RELAY_MESSENGER_VERIFY_TOKEN=<secret>

META_RELAY_REVIEW_ORGANIZATION_ID=<same organization UUID>
META_RELAY_REVIEW_META_BUSINESS_ID=<same Page-owning Business ID>
META_RELAY_REVIEW_PAGE_ID=<same Messenger Page ID>
META_RELAY_REVIEW_CHANNEL_ACCOUNT_ID=<same account UUID>
META_RELAY_REVIEW_GENERATION=<same review-generation UUID>
META_RELAY_REVIEW_EXPIRES_AT=<same expiry string>
META_RELAY_REVIEW_BROKER_AUTH_SECRET=<same broker-auth secret>
META_RELAY_REVIEW_BROKER_WRAP_SECRET=<same broker-wrap secret>
```

The following rules are enforced at startup:

- remote Redis must use `rediss://`; plaintext `redis://` is accepted only for
  loopback, and `unix://` is also accepted;
- `META_RELAY_REDIS_PREFIX`, when explicitly set, must be exactly
  `rereply:meta-relay:review:<generation>:`. Leaving it unset derives that
  isolated prefix automatically;
- `META_RELAY_REVIEW_BINDING_CACHE_TTL` defaults to `2s`, must be positive,
  and cannot exceed `10s`;
- the Messenger app secret, verify token, provider-proof, broker-auth, and
  broker-wrap secrets must each be at least 32 bytes, contain no whitespace,
  and be pairwise distinct; and
- the processing lease must cover the forwarding timeout, queue settlement,
  and safety margin. The normal relay duration and worker settings remain
  available but must be positive.

Do not declare `META_RELAY_ACCOUNTS_JSON`. Review bindings come only from the
broker. Also omit all `META_RELAY_INSTAGRAM_*` variables and these production
App Review evidence variables:

```text
META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS
META_RELAY_MESSENGER_REVIEWED_BY
META_RELAY_MESSENGER_REVIEWED_AT
META_RELAY_MESSENGER_REVIEW_EVIDENCE
```

The runtime refuses an `approved` App Review claim. Tech Provider verification
and App Review approval are different facts; `verified` is required for the
former while the latter must remain truthful and unapproved in this runtime.

## Shared-value matrix

Provision these pairs from the same secret-manager entries or reviewed
deployment source. Never compare or print their values in CI.

| ReReply service | Review relay service | Relationship |
| --- | --- | --- |
| `WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID` | `META_RELAY_MESSENGER_APP_ID` | Exact match |
| `WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET` | `META_RELAY_MESSENGER_APP_SECRET` | Same opaque Meta secret |
| `WHATOMATE_META_MESSENGER_REVIEW_RELAY__REREPLY_BASE_URL` | `META_RELAY_REREPLY_BASE_URL` | Exact match |
| `WHATOMATE_META_MESSENGER_REVIEW_RELAY__PROVIDER_PROOF_SECRET` | `META_RELAY_REREPLY_PROVIDER_PROOF_SECRET` | Same opaque secret |
| `WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_AUTH_SECRET` | `META_RELAY_REVIEW_BROKER_AUTH_SECRET` | Same opaque secret |
| `WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_WRAP_SECRET` | `META_RELAY_REVIEW_BROKER_WRAP_SECRET` | Same opaque secret |
| Review organization, Business, Page, account, generation, and expiry fields | Corresponding `META_RELAY_REVIEW_*` fields | Exact string match |

`WHATOMATE_APP__ENCRYPTION_KEY` remains ReReply-only. The Page token is created
by authenticated onboarding and stays encrypted in ReReply storage; there is
no relay environment variable for it. Likewise, do not invent inbound or
outbound account-secret environment variables for review mode.

Each broker request carries domain-separated, non-secret fingerprints of the
relay's Messenger App Secret and provider-proof secret. ReReply compares them
with its own configured values before Redis or tenant storage is touched, then
repeats them inside the authenticated encrypted bundle. A one-sided rotation
therefore makes normal webhook acceptance and `GET /reviewz` fail closed; it
never produces a falsely ready binding.

## Deployment and review checklist

1. Confirm that the work is a time-bounded Meta App Review exercise using a
   non-production ReReply deployment, Redis, database, domain, Messenger app,
   Business, and Page.
2. Record the exact Tech Provider app owner separately from the Page-owning
   Business. Confirm that the app really is in the declared Development or
   Live/Published mode and that its App Review state is one of the accepted
   unapproved values. A Live declaration is transport availability for the
   app-role reviewer only; it must not be recorded as approval evidence.
3. Generate the future expiry, the fixed ChannelAccount UUID, a new generation
   UUID, and three independent high-entropy broker/proof secrets. Keep an
   operator record of identifiers only; keep secret values in the secret
   manager.
4. Configure ReReply and the relay from the tables above. Verify the tuple,
   URL strings, app ID, and shared-secret references before deployment.
5. Confirm there are no normal `WHATOMATE_META_RELAY__*` trust values, static
   `META_RELAY_ACCOUNTS_JSON`, Instagram variables, or false App Review
   approval/evidence values in either staging service.
6. Deploy both staging services. Check `GET /livez` and `GET /readyz` on the
   relay. These prove process, Redis, and provider-proof availability; they do
   not prove that onboarding has created the pinned account. Before onboarding,
   `GET /reviewz` is expected to return a generic `503`.
7. Configure Meta's Messenger callback as
   `<RELAY_BASE_URL>/v1/meta/messenger/webhook` and use the exact relay verify
   token. Complete callback verification.
8. While signed into the configured ReReply organization, complete the
   authenticated onboarding start, exchange, and select flow. The server
   exposes only the configured Business and Page and creates or resumes only
   the pre-pinned ChannelAccount UUID.
9. Confirm the account reports `pending`, no connected timestamp,
   `review_relay_ready`, `review_only=true`, `registry_recognized=false`, and
   disabled outbound/AI/default-outgoing state. Then require `GET /reviewz` to
   return `204`; this is the exact dynamic-binding readiness check.
10. Send a new text DM from the single app-role tester permitted by the review
    setup. Confirm that the event appears only in the intended staging
    workspace and opens a 24-hour customer-service window. Live/Published mode
    does not authorize ordinary customer traffic without approved access.
11. Sign in as the exact configured reviewer. Confirm that the dedicated
    review-reply eligibility check names the exact Page and recipient. Enter
    one plain-text response, acknowledge the manual-send warning, and submit it
    once. Confirm that the same sender receives it in Messenger. Ordinary
    outbound, media, AI, mark-read, and default routing must remain disabled.
12. Confirm the relay has not registered Instagram, account-health, or
    outbound routes and that application logs contain no request signatures,
    nonces, credential bundles, Page tokens, webhook HMACs, or provider-proof
    values.
13. Before the authority expires, run the dedicated deprovision flow below.
    Do not leave an expired review row or Meta subscription for later cleanup.

Changing any review authority or secret changes the onboarding session
fingerprint and invalidates an in-flight session. Treat each generation as an
immutable, coordinated staging grant. Do not change one runtime independently
or convert the review row into a production connection in place.

## Deprovisioning and failure recovery

Generic ChannelAccount deletion is blocked. An authenticated operator with
ChannelAccount delete permission must call:

```text
DELETE /api/integrations/meta/messenger/review/{channel_account_id}
```

The dedicated sequence is quarantine-first:

1. Before the remote subscribe call, ReReply persists a bounded operation
   lease tied to the exact OAuth credential ID and version. Under the
   tenant/account lock, deprovisioning changes the desired state to
   `unsubscribed` and fences any still-running subscribe operation.
2. Under the same lock, ReReply changes the account to
   `disconnected`, clears default routing, disables outbound and AI, changes
   state to `review_deprovisioning`, revokes and erases the webhook credential,
   and cancels queued AI and outbox work.
   If a dedicated review reply has already crossed its durable dispatch fence,
   this step returns a retryable draining response before quarantine or
   credential erasure. Retry only after the recorded provider attempt reaches a
   deterministic terminal settlement. A crashed or unsettled permanent fence
   never clears by elapsed time and requires a separately authorized, audited
   operator reconciliation before deprovisioning can continue.
3. Only after that local fence commits, ReReply uses the still-encrypted Page
   token to delete the exact Page's `subscribed_apps` binding from Meta.
   A separate `GET /subscribed_apps` absence check resolves a timed-out or
   otherwise ambiguous DELETE.
4. A late subscribe response must pass the exact operation and OAuth-generation
   compare-and-swap before it can become ready. A stale response instead runs a
   compensating DELETE plus the same absence check and records its durable
   acknowledgement.
5. After Meta confirms success and any fenced subscribe has acknowledged
   compensation or its lease has expired, ReReply revokes and erases all remaining
   credentials, including the OAuth Page token, records
   `review_deprovisioned`, soft-deletes the account, and writes the audit event.

If Meta unsubscribe fails, the account remains locally quarantined in
`review_remote_cleanup_pending`; inbound authority is already revoked, while
the Page token is retained only so the same operation can be retried. If Meta
cleanup succeeds but local erasure fails, retry local completion immediately.
Never restore the webhook credential merely to make cleanup easier, manually
soft-delete the row, or treat a cleanup-pending account as safe to promote.
`GET /reviewz` must return the same generic `503` as soon as quarantine begins.

Recovery is deliberately request-driven in this staging-only implementation:
there is no background cleanup reconciler. Repeat the same authenticated
deprovision request, or have an operator retry it, until the durable tombstone
reports success. A corrupt or missing operation fence fails closed and requires
operator investigation; do not bypass it or erase the retained cleanup token.

Broker, Redis, ReReply, or credential-binding failures return a fail-closed
error. Restore the affected staging dependency or correct the exact
deployment tuple; do not bypass signatures, relax the expiry, add a static
relay account, or copy the Page token to the relay.

## Production prohibition and promotion

ReReply startup accepts this feature only when
`WHATOMATE_APP__ENVIRONMENT` is exactly `staging`. The relay independently
requires both:

```text
META_RELAY_RUNTIME_MODE=staging_messenger_review
META_RELAY_DEPLOYMENT_ENVIRONMENT=staging
```

The production preflight in `scripts/meta_relay_preflight.py` scans every
service, worker, and job. It rejects review wiring by key presence, even when a
value is empty or `ENABLED=false`:

- every `WHATOMATE_META_MESSENGER_REVIEW_RELAY__*` key;
- every `META_RELAY_REVIEW_*` key;
- `META_RELAY_RUNTIME_MODE=staging_messenger_review`; and
- the abandoned alias `META_RELAY_MODE=staging_messenger_review`.

Do not weaken or skip this gate. Normal production App Review governance
variables such as `META_RELAY_MESSENGER_REVIEWED_AT` remain valid because they
do not use the reserved review-runtime prefix.

Production requires the normal Live-mode relay, approved Advanced Access,
protected account inventories, signed readiness probes, recurring ownership
revalidation, and the reviewed activation/deprovision process in
[Meta relay deployment preflight](meta-relay-preflight.md). Deprovision this
staging review binding first and provision production through that normal
path. A successful App Review demonstration does not make this temporary
runtime or its pending account production-ready.
