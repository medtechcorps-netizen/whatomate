# Meta Relay

The Meta relay is the durable, text-only Messenger and Instagram transport for
ReReply relay channel accounts. It is a separate process built from
`./cmd/meta-relay` and uses Redis as its acceptance, queue, and idempotency
boundary.

## Webhook applications and routes

Configure distinct Meta applications and do not reuse an app secret
or a verify token:

| Meta application | Callback route | Accepted accounts |
| --- | --- | --- |
| Legacy Messenger parent app(s) | `GET`/`POST /v1/meta/messenger/webhook` | Existing static Messenger Pages and Instagram accounts using `facebook_login` |
| Managed Messenger platform app | `GET`/`POST /v1/meta/messenger/managed-webhook` | Registry-managed Messenger Pages only |
| Existing static Instagram Login app | `GET`/`POST /v1/meta/instagram/webhook` | Existing static Instagram Login mappings only |
| Managed Instagram Login platform app | `GET`/`POST /v1/meta/instagram/managed-webhook` | Registry-managed Instagram professional profiles only |

The shared legacy route `/v1/meta/webhook` does not exist. Each POST route
validates `X-Hub-Signature-256` only with that route's app secret. Each GET
route validates only that route's verify token. A correctly signed payload is
still rejected if its account is bound to the other application.

Behind the production `/meta-relay` ingress prefix, configure Meta's managed
Messenger callback as
`https://app.rereply.app/meta-relay/v1/meta/messenger/managed-webhook`. Keep
the legacy callback route unchanged. The managed route must use its own exact
app ID, app secret, and verify token; none of these values may be inferred from
the mixed legacy static mappings.

Additional routes:

| Route | Purpose |
| --- | --- |
| `GET /livez` | Process liveness only |
| `GET /readyz` | Redis-backed service readiness |
| `HEAD /v1/accounts/{channel}/{external_account_id}` | Signed Redis plus Graph credential/account health check |
| `POST /v1/accounts/{channel}/{external_account_id}` | Signed ReReply outbound text delivery |

ReReply signs the exact outbound body with the account's
`rereply_outbound_secret_env` value in
`X-ReReply-Signature-256: sha256=<hex-hmac>`. The HEAD request signs an empty
body with the same secret. The relay signs canonical events sent to ReReply
with the account's `rereply_inbound_secret_env` value.

## Required environment

All deployments require:

| Variable | Meaning |
| --- | --- |
| `META_RELAY_REDIS_URL` | Redis URL. There is no in-memory durability fallback. |
| `META_RELAY_MESSENGER_APP_SECRET` | App secret for the Messenger parent app webhook route. |
| `META_RELAY_MESSENGER_VERIFY_TOKEN` | Verify token for the Messenger parent app callback. |
| `META_RELAY_MANAGED_MESSENGER_APP_ID` | Exact platform-app ID for registry-managed Messenger bindings. Required only when the dynamic registry is enabled. |
| `META_RELAY_MANAGED_MESSENGER_APP_SECRET` | Distinct app secret for the managed Messenger callback and `appsecret_proof`. Required only when the dynamic registry is enabled. |
| `META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN` | Distinct verify token for the managed Messenger callback. Required only when the dynamic registry is enabled. |
| `META_RELAY_INSTAGRAM_APP_SECRET` | App secret for the separate Instagram Login app webhook route. |
| `META_RELAY_INSTAGRAM_VERIFY_TOKEN` | Verify token for the Instagram Login app callback. |
| `META_RELAY_MANAGED_INSTAGRAM_APP_ID` | Exact platform-app ID for registry-managed Instagram Login bindings. |
| `META_RELAY_MANAGED_INSTAGRAM_APP_SECRET` | Distinct managed Instagram app secret for its webhook route and `appsecret_proof`. |
| `META_RELAY_MANAGED_INSTAGRAM_VERIFY_TOKEN` | Distinct verify token for the managed Instagram callback. |
| `META_RELAY_GRAPH_API_VERSION` | Explicit version such as `v25.0`; update deliberately when Meta support changes. |
| `META_RELAY_ACCOUNTS_JSON` | Optional static fallback account mappings. It contains environment-variable names, never secret values. Existing mappings continue to take precedence during migration. |
| `META_RELAY_REGISTRY_ENABLED` | Explicit dynamic-registry gate. Defaults to `false`. |
| `META_RELAY_REGISTRY_URL` | Exact private dynamic broker endpoint. |
| `META_RELAY_REGISTRY_SECRET` | Broker service-authentication secret (minimum 32 bytes). |
| `META_RELAY_REGISTRY_EDGE_SECRET` | Separate outer credential checked before dynamic public-route lookup (minimum 32 bytes). |
| `META_RELAY_DYNAMIC_QUEUE_READER_VERSION` | Must be `2` on every relay reader before dynamic producers are enabled. |

The legacy Messenger and Instagram webhook credentials must always be
configured and their app secrets and verify tokens remain distinct from each
other. When a managed product is enabled, its app ID, secret, and verify token
are required and must also differ from every static or other managed app
credential. A managed Instagram app ID may never equal the managed Messenger
app ID.

Optional tuning:

| Variable | Default | Notes |
| --- | --- | --- |
| `META_RELAY_LISTEN_ADDR` | `:8081` | HTTP listen address. |
| `META_RELAY_REDIS_PREFIX` | `rereply:meta-relay:` | Dedicated Redis key prefix. |
| `META_RELAY_INBOUND_RETENTION` | `168h` | Acceptance/completion/dead-letter retention. |
| `META_RELAY_OUTBOUND_RETENTION` | `168h` | Outbound idempotency retention. |
| `META_RELAY_PROCESSING_LEASE` | `60s` | Claimed inbound-job lease. |
| `META_RELAY_FORWARD_TIMEOUT` | `15s` | Maximum ReReply forwarding attempt. |
| `META_RELAY_WORKER_CONCURRENCY` | `4` | Jobs claimed and started together; maximum `64`. |
| `META_RELAY_POLL_INTERVAL` | `500ms` | Queue polling interval. |
| `META_RELAY_MAX_ATTEMPTS` | `12` | ReReply forwarding attempt budget. |
| `META_RELAY_REGISTRY_CACHE_TTL` | `10s` | Maximum ingress-only dynamic binding cache; cannot exceed one minute or the broker lease. |
| `META_RELAY_REGISTRY_TIMEOUT` | `3s` | Dynamic broker request timeout; cannot exceed ten seconds. |

Static mappings remain checked first and are not modified by managed
enablement. The dynamic registry stays default-off and fails startup unless its
URL, both service credentials, exact managed app binding, and queue reader v2
gate are complete and mutually consistent.

That precedence is also a managed-Instagram cutover invariant. Before any
managed OAuth callback or `/subscribed_apps` mutation, inspect the effective
`META_RELAY_ACCOUNTS_JSON` on every replica and prove the intended profile ID
is absent. A database channel deletion does not remove an environment mapping;
leaving the profile there routes outbound through the old static token and
prevents the managed webhook app from owning ingress. The current four static
production mappings remain unchanged for this rollout, so the pilot profile
must not overlap them. A future overlapping migration requires a separate
drain: block static intake, drain/settle old static work, remove the exact
mapping from every replica, roll all replicas, then start managed OAuth.

## Dynamic control plane

> **Production gate:** keep the dynamic registry and Messenger lifecycle off
> until the managed app identity has been cryptographically verified, every
> relay replica is a queue-v2 reader, and the ReReply deployment has its exact
> pilot organization allowlist configured. An empty allowlist fails closed.

Dynamic Messenger and Instagram accounts use separate platform-owned
applications. A workspace authorizes its Page or professional profile; it does
not supply an app secret. Each webhook request is signed by the application
that owns the subscription, so each managed product has an isolated route in
addition to the untouched static routes. See
[Managed Instagram Login lifecycle](meta-instagram-managed-lifecycle.md) for
the release-evidence and activation contract.

The ReReply API remains the only process with the PostgreSQL credential and
application encryption key. The relay never receives either. Instead it calls
the private registry broker using an HMAC request bound to method, exact path,
timestamp, one-time nonce, and body digest. ReReply rejects replayed nonces in
Valkey and signs the exact status/body of every response.

The broker resolves a globally unique `(channel, external_account_id)` to one
organization, enters that tenant's RLS transaction, and returns a short secret
lease only when all of the following remain true:

- the account is active, `relay`-backed, and explicitly marked
  `meta_registry_managed` with `meta_management_mode=platform_oauth`;
- ownership is `verified`, has not exceeded the configured maximum age, and no
  deauthorization timestamp exists;
- both the OAuth token credential and the independent webhook-signing
  credential are active, encrypted, and versioned; and
- the ReReply webhook target is absolute HTTPS.

Ingress may cache that lease for a few seconds. Queue forwarding, account
health, and outbound sends re-resolve without cache. Relay queue schema 2
stores both credential-version fences. ReReply-managed outbox jobs do not
carry those relay fields, so every managed credential-generation change
transactionally cancels all pending, retrying, and processing outbox work
before it can resolve a newer lease. A job that already reached `dispatching`
retains the provider-attempt win. Together these fences prevent old work from
being delivered with new authority. Registry responses and credentials must never be logged,
persisted by the relay, exposed to browser APIs, or copied into audit rows.

A stale ownership binding is distinct from a revoked or missing binding. New
ingress is rejected with a retryable service response, and an already durable
job is parked without consuming its delivery retry budget. Revoked, missing,
or credential-version-mismatched jobs remain dead-letter outcomes.

Deauthorization and ownership-revalidation mutations use the same service
authentication and compare-and-swap both credential IDs and versions. A
revocation disconnects the channel and revokes both credential envelopes in
one tenant transaction. Audit events contain state and version metadata only.

The processing lease must be at least the registry timeout (when dynamic), the
forward timeout, the five-second queue settlement budget, and a one-second
safety margin. The worker claims no
more than its concurrency and starts the full claimed batch concurrently; it
does not lease a large sequential backlog.

Dynamic health and outbound routes also require the deployment-owned outer
service credential before account resolution. The per-account body HMAC is
still verified after resolution. This prevents unauthenticated requests from
amplifying into broker, database, and decryption work.

## Reader-first rollout and rollback

The current four production mappings stay in `META_RELAY_ACCOUNTS_JSON`; no
database import or environment change is part of this patch, and the managed
pilot must be a different profile. Queue schema 0/1 static jobs remain
readable, new static jobs carry schema 2, and an unknown future schema is
parked instead of destroyed. Before the later lifecycle release enables a
dynamic producer, every relay replica must first run a dynamic-aware schema-v2
reader and the old-worker queue must be drained. Enable the managed lifecycle
only after the exact intended web and relay SHA is running everywhere. Once
the first managed binding or managed queue job exists, rolling back to an old
binary is prohibited because it cannot honor lifecycle fences and can reopen
legacy manual Messenger creation. Before any later rollback, disable intake,
deploy the new binary with `quarantine_only=true`, require every API and worker
replica to complete the synchronous pre-serve/pre-claim tenant quarantine and
queue-cancellation barrier, and fail startup if that commit cannot complete.
Do not wait for an asynchronous lifecycle sweep; reconcile
unsubscribe/disconnect operations, and drain or safely park all managed jobs
with the new reader. If reverting the profile to static, add its mapping only
after those fences settle and deploy the mapping consistently to every
replica; static lookup immediately regains precedence. Keep a compatible
data-deletion callback and status responder online for every retained
confirmation code; an old web binary cannot fulfill that obligation merely
because it ignores the additive table.

## Account and token binding

Every Instagram mapping must set `instagram_api_mode`. It is not inferred from
the channel, token, webhook payload, or environment-variable name.

| Channel/mode | Required token | Graph host | Webhook application |
| --- | --- | --- | --- |
| `messenger` | Facebook Page token with Messenger permissions | `graph.facebook.com` | Messenger parent app |
| `instagram` + `instagram_login` | Instagram user or system-user token with `instagram_business_*` permissions, including `instagram_business_manage_messages` | `graph.instagram.com` | Separate Instagram Login app |
| `instagram` + `facebook_login` | Facebook Page token with `instagram_*` permissions, including `instagram_manage_messages` | `graph.facebook.com` | Messenger parent app |

Messenger mappings must omit `instagram_api_mode`. A missing or unrecognized
Instagram mode, or a mode on a Messenger mapping, stops startup.

Example:

```json
[
  {
    "key": "support-page",
    "channel": "messenger",
    "external_account_id": "FACEBOOK_PAGE_ID",
    "rereply_webhook_url": "https://app.example.com/api/webhooks/channels/ACCOUNT_ID",
    "access_token_env": "META_SUPPORT_PAGE_TOKEN",
    "rereply_inbound_secret_env": "REREPLY_SUPPORT_INBOUND_SECRET",
    "rereply_outbound_secret_env": "REREPLY_SUPPORT_OUTBOUND_SECRET"
  },
  {
    "key": "instagram-direct",
    "channel": "instagram",
    "external_account_id": "INSTAGRAM_USER_ID",
    "instagram_api_mode": "instagram_login",
    "rereply_webhook_url": "https://app.example.com/api/webhooks/channels/ACCOUNT_ID",
    "access_token_env": "META_INSTAGRAM_USER_TOKEN",
    "rereply_inbound_secret_env": "REREPLY_INSTAGRAM_INBOUND_SECRET",
    "rereply_outbound_secret_env": "REREPLY_INSTAGRAM_OUTBOUND_SECRET"
  },
  {
    "key": "instagram-via-page",
    "channel": "instagram",
    "external_account_id": "INSTAGRAM_BUSINESS_ACCOUNT_ID",
    "instagram_api_mode": "facebook_login",
    "rereply_webhook_url": "https://app.example.com/api/webhooks/channels/ACCOUNT_ID",
    "access_token_env": "META_INSTAGRAM_PAGE_TOKEN",
    "rereply_inbound_secret_env": "REREPLY_INSTAGRAM_PAGE_INBOUND_SECRET",
    "rereply_outbound_secret_env": "REREPLY_INSTAGRAM_PAGE_OUTBOUND_SECRET"
  }
]
```

At account health time, the relay first pings Redis and then probes the exact
mode-bound Graph host using the token only in the `Authorization` header:

- Messenger requires `GET /{version}/me?fields=id` to return the configured
  Page ID.
- Instagram Login requires
  `GET /{version}/me?fields=user_id` to return the configured Instagram user
  ID.
- Facebook Login for Instagram requires
  `GET /{version}/me?fields=id,instagram_business_account{id}` to return the
  configured Instagram business account under the Page token.

Expired tokens, provider errors, malformed responses, wrong accounts, and
wrong token families all fail health with a generic response. Tokens and
provider response bodies are never logged or returned.

Registry-managed Messenger Graph health, subscription checks, and outbound
sends include `appsecret_proof` made with the distinct managed app secret and
require the binding's exact managed platform-app ID. The static compatibility
path intentionally does not add that proof: legacy Page tokens may have been
issued by different clinic-era apps and must not be silently rebound or broken
by this rollout.

## Delivery safety

Inbound Meta webhooks are acknowledged only after the full normalized delivery
is durably accepted in Redis. Repeated deliveries use the raw-body digest as
their acceptance key, while individual canonical events carry stable provider
dedupe keys.

Outbound requests require an account-scoped idempotency key. Successful and
definitive 4xx outcomes are cached. An explicit `429` releases the claim for a
later retry. A Graph transport error, any Graph `5xx`, an invalid success
response, or a failure to durably record a success is persisted as
`delivery_outcome_ambiguous`; the same key is never blindly sent again.

## Build and run

```sh
docker build -f docker/meta-relay.Dockerfile -t rereply-meta-relay .
docker run --rm --env-file meta-relay.env -p 8081:8081 rereply-meta-relay
```

The image health check uses `/readyz`. ReReply should separately call each
configured account's signed HEAD route so expired or misbound Graph
credentials are detected.
