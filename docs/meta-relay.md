# Meta Relay

The Meta relay is the durable, text-only Messenger and Instagram transport for
ReReply relay channel accounts. It is a separate process built from
`./cmd/meta-relay` and uses Redis as its acceptance, queue, and idempotency
boundary.

New Messenger Pages must be staged through the ownership-verifying flow in
[Managed Messenger onboarding](meta-messenger-onboarding.md); a relay mapping
or similarly named Page is not sufficient evidence that a workspace owns the
asset.

Use the reusable
[Meta asset connection runbook](meta-asset-connection-runbook.md) to diagnose
cross-workspace differences and onboard future organizations without relying
on display names or outbound-only tests.

Meta App Review uses a separate, deliberately inbound-only runtime documented
in the [staging-only Messenger review relay runbook](meta-messenger-review-relay.md).
That mode is not a production relay profile and cannot share a process or
deployment configuration with the protected production account registry.

## Webhook applications and routes

Configure two distinct Meta applications and do not reuse either an app secret
or a verify token:

| Meta application | Callback route | Accepted accounts |
| --- | --- | --- |
| Messenger parent app | `GET`/`POST /v1/meta/messenger/webhook` | Messenger Pages and Instagram accounts using `facebook_login` |
| Separate Instagram Login app | `GET`/`POST /v1/meta/instagram/webhook` | Instagram accounts using `instagram_login` only |

The shared legacy route `/v1/meta/webhook` does not exist. Each POST route
validates `X-Hub-Signature-256` only with that route's app secret. Each GET
route validates only that route's verify token. A correctly signed payload is
still rejected if its account is bound to the other application.

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

The relay additionally signs canonical Meta events and readiness-v2 responses
with the deployment-held `META_RELAY_REREPLY_PROVIDER_PROOF_SECRET` in
`X-ReReply-Meta-Provider-Proof-256`. ReReply verifies it using
`WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET`. This proof is domain-separated
between inbound bodies and readiness mappings; it is not an account credential
and must never enter tenant configuration or an API response.

## Required environment

All deployments require:

| Variable | Meaning |
| --- | --- |
| `META_RELAY_REDIS_URL` | Redis URL. There is no in-memory durability fallback. |
| `META_RELAY_REREPLY_BASE_URL` | Canonical HTTPS ReReply origin, without a path, query, fragment, or credentials. Every account webhook must be exactly below this origin. Production uses `https://app.rereply.app`. |
| `META_RELAY_REREPLY_PROVIDER_PROOF_SECRET` | Deployment-held provider-proof key shared only with ReReply. Store as `SECRET`; it must contain at least 32 UTF-8 bytes with no surrounding whitespace. |
| `META_RELAY_MESSENGER_APP_SECRET` | App secret for the Messenger parent app webhook route. |
| `META_RELAY_MESSENGER_APP_ID` | App ID expected in every Messenger/Facebook Login asset subscription. |
| `META_RELAY_MESSENGER_APP_MODE` | Must resolve to exact `live`; Development mode cannot prove service for external organizations. |
| `META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID` | Numeric Meta Business Portfolio ID that owns the Messenger app. It may equal the Instagram app owner's ID. |
| `META_RELAY_MESSENGER_TECH_PROVIDER_STATUS` | Must be exactly `verified`, recorded separately from App Review. |
| `META_RELAY_MESSENGER_APP_REVIEW_STATUS` | Must be exactly `approved`; set only after verifying Live mode and Advanced Access for every permission recorded below. |
| `META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS` | Exact comma-separated Advanced Access set. Relay delivery needs `pages_messaging,pages_manage_metadata`. Production preflight is intentionally stricter and also requires `pages_show_list,pages_read_engagement,business_management` so future organizations can be discovered and ownership-reviewed. Facebook Login Instagram mappings additionally require `instagram_basic,instagram_manage_messages`. |
| `META_RELAY_MESSENGER_REVIEWED_BY` | Reviewer identity in `Name <email>` format for the current ownership, Tech Provider, and App Review audit. |
| `META_RELAY_MESSENGER_REVIEWED_AT` | UTC RFC3339 timestamp ending in `Z` for that audit. |
| `META_RELAY_MESSENGER_REVIEW_EVIDENCE` | HTTPS URL without credentials, query, or fragment for the protected audit evidence. The relay validates but never fetches it. |
| `META_RELAY_MESSENGER_VERIFY_TOKEN` | Verify token for the Messenger parent app callback. |
| `META_RELAY_INSTAGRAM_APP_SECRET` | App secret for the separate Instagram Login app webhook route. |
| `META_RELAY_INSTAGRAM_APP_ID` | App ID expected in every Instagram Login asset subscription. |
| `META_RELAY_INSTAGRAM_APP_MODE` | Must resolve to exact `live`; Development mode cannot prove service for external organizations. |
| `META_RELAY_INSTAGRAM_APP_OWNER_BUSINESS_ID` | Numeric Meta Business Portfolio ID that owns the Instagram Login app. It may equal the Messenger app owner's ID. |
| `META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS` | Must be exactly `verified`, recorded separately from App Review. |
| `META_RELAY_INSTAGRAM_APP_REVIEW_STATUS` | Must be exactly `approved`; set only after verifying Live mode and required advanced-access permissions in Meta App Review. |
| `META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS` | Comma-separated permissions verified as approved. Instagram Login mappings require `instagram_business_basic,instagram_business_manage_messages`. |
| `META_RELAY_INSTAGRAM_REVIEWED_BY` | Reviewer identity in `Name <email>` format for the current ownership, Tech Provider, and App Review audit. |
| `META_RELAY_INSTAGRAM_REVIEWED_AT` | UTC RFC3339 timestamp ending in `Z` for that audit. |
| `META_RELAY_INSTAGRAM_REVIEW_EVIDENCE` | HTTPS URL without credentials, query, or fragment for the protected audit evidence. The relay validates but never fetches it. |
| `META_RELAY_INSTAGRAM_VERIFY_TOKEN` | Verify token for the Instagram Login app callback. |
| `META_RELAY_GRAPH_API_VERSION` | Explicit version such as `v25.0`; update deliberately when Meta support changes. |
| `META_RELAY_ACCOUNTS_JSON` | Account mappings. It contains environment-variable names, never secret values. |

The four webhook credential values must all be configured. The two app secrets,
app IDs, and verify tokens must differ. The two app-owner Business Portfolio IDs
may be the same when one verified Tech Provider portfolio owns both apps.

DigitalOcean must declare the Redis URL, both app secrets, both verify tokens,
the provider-proof value, and every environment variable referenced by an
account's `access_token_env`, `rereply_inbound_secret_env`, or
`rereply_outbound_secret_env` exactly once as a non-empty `SECRET`. The
pre-mutation structural preflight checks declaration and type without exposing
or comparing any value. `META_RELAY_ACCOUNTS_JSON` itself remains inspectable
`GENERAL` configuration because it contains only IDs, URLs, and secret-variable
names.

The ReReply application service has its own deployment-held trust copy:

| Variable | Meaning |
| --- | --- |
| `WHATOMATE_META_RELAY__BASE_URL` | Canonical approved public relay prefix. Production uses `https://app.rereply.app/meta-relay`; tenant settings cannot override this trust anchor. |
| `WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON` | The same complete protected inventory document used by deployment preflight. ReReply derives each account's exact organization, asset, and Meta Business binding from this copy. |
| `WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET` | The same deployment-held provider-proof key used by the relay. Store as `SECRET`; never expose it through tenant configuration or APIs. |

The base URL and inventory are non-secret `GENERAL` configuration. The provider
proof is a `SECRET`. Deployment preflight checks the ReReply inventory copy,
both runtime base URLs, and the presence/type of both secret declarations before
the protected-main workflow changes production. Its live phase compares the
domain-separated `X-ReReply-Meta-Provider-Proof-Key-ID` fingerprints emitted by
ReReply `/ready` and the relay account probes; it never retrieves or prints the
secret values.

Because exported secrets are opaque, CI proves both services use the same key
through that HMAC fingerprint rather than through secret export. After every
provider-proof rotation, still run Test for every mapped account;
only a cryptographically valid readiness proof writes server-only
`meta_provider_proof_version: "v1"`. Then obtain a fresh post-Test external
customer text DM and re-approve outbound. A passing key-fingerprint monitor does
not replace that rotation test.

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

The processing lease must be at least the forward timeout plus the five-second
queue settlement budget and a one-second safety margin. The worker claims no
more than its concurrency and starts the full claimed batch concurrently; it
does not lease a large sequential backlog.

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
    "organization_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
    "meta_business_id": "200000000000001",
    "channel": "messenger",
    "external_account_id": "FACEBOOK_PAGE_ID",
    "rereply_webhook_url": "https://app.example.com/api/webhooks/channels/ACCOUNT_ID",
    "access_token_env": "META_SUPPORT_PAGE_TOKEN",
    "rereply_inbound_secret_env": "REREPLY_SUPPORT_INBOUND_SECRET",
    "rereply_outbound_secret_env": "REREPLY_SUPPORT_OUTBOUND_SECRET"
  },
  {
    "key": "instagram-direct",
    "organization_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
    "meta_business_id": "200000000000001",
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
    "organization_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2",
    "meta_business_id": "200000000000002",
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

The relay then calls
`GET /{version}/{external_account_id}/subscribed_apps?fields=id,subscribed_fields`
on the same mode-bound Graph host. Health passes only when the response contains
the configured webhook app ID with the `messages` field. This detects an app
whose callback exists but whose Page or Instagram professional account was
never subscribed (or was later unsubscribed).

Every mapping must contain the canonical ReReply `organization_id` UUID and the
numeric `meta_business_id` of the reviewed Business Portfolio that owns the
Page or Instagram professional account. Every `rereply_webhook_url` must be the exact
`/api/webhooks/channels/{channel_account_id}` production URL without a query.
After all checks pass, the readiness-v2 health endpoint attests the organization
UUID, channel-account UUID, channel, external asset ID, and Meta Business
Portfolio ID. Before marking the connection active, ReReply compares that full
tuple—including the Business ID—with its deployment-held protected inventory.
Deployment preflight separately proves that ReReply's copy matches the GitHub
production inventory. Another tenant's mapping or a generic healthy endpoint
cannot satisfy Test.

`*_TECH_PROVIDER_STATUS=verified` and `*_APP_REVIEW_STATUS=approved` are
separate deployment gates, not Meta API substitutes. Record them only from a
current audit that also confirms the numeric app-owner Business Portfolio, Live
mode, and permissions required by the app's login mode. The reviewer, UTC audit
timestamp, and protected HTTPS evidence URL are mandatory per app. The relay
validates but never fetches that URL. Missing or unverified evidence stops relay
startup instead of allowing new tenant connections to appear operational.
`reviewed_at` must not be more than five minutes in the future and expires after
90 days; refresh the review record and evidence before that deadline.

The relay runtime minimum for Messenger delivery is only `pages_messaging` and
`pages_manage_metadata`. That is not enough to declare the installation ready
for future organizations. The protected production preflight additionally
requires Advanced Access for `pages_show_list`, `pages_read_engagement`, and
`business_management`. This five-permission onboarding profile prevents a
working existing Page from masking a Page-discovery or Business-ownership
failure for the next organization.

### Safe rollout order

1. Audit the Messenger parent app and the separate Instagram Login app. Do not
   set Tech Provider status to `verified` or App Review status to `approved`
   without current owner, permission, reviewer, timestamp, and evidence records.
2. Prepare a local candidate DigitalOcean app spec containing both app IDs,
   app-owner Business Portfolio IDs, separate status/evidence variables, and
   each mapping's canonical organization UUID and reviewed asset owner Business
   Portfolio ID. Replace any non-canonical `rereply_webhook_url` with the exact
   production channel-account UUID URL. Copy the complete protected inventory
   into `WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON`, and set both runtime base
   URL trust anchors to their exact production values.
3. Run `scripts/meta_relay_preflight.py --app-spec <candidate> --validate-only`
   against the candidate before `doctl apps update` or an equivalent console
   save. The GitHub gate cannot retroactively protect a direct spec mutation
   that happened before the workflow started.
4. Let the production workflow run the protected inventory comparison again with
   `--validate-only` before that workflow performs its DigitalOcean spec update
   or protected-main deployment. A missing service, app declaration, Advanced
   Access permission, or mapping stops the code release.
5. Confirm each mapped asset's `subscribed_apps` response contains the matching
   app ID and `messages` before deploying the updated Meta relay.
6. Deploy the Meta relay and ReReply strict validator together as one compatible
   DigitalOcean app release.
7. After DigitalOcean reports the deployment ready, require the signed-header
   account preflight to pass for every mapping before running Test on any tenant
   connection or approving outbound delivery.
8. After a connection passes Test, send a new incoming **text** DM from a
   genuine external customer profile that is not listed as a Meta app admin,
   developer, or tester. It must arrive strictly after that Test. An app-role
   profile can work in Development mode and is not production evidence. Refresh
   ReReply and approve outbound only after this provider-proven event appears;
   attachment-only photos, stickers, reactions, older messages, and replayed
   events do not count as phase-one webhook proof.

Starting Test/Re-certify blocks new and queued outbound/AI delivery. A provider
request already dispatching across the network may still complete; it cannot be
revoked after Meta receives it.

Expired tokens, provider errors, malformed responses, wrong accounts, and
wrong token families all fail health with a generic response. Tokens and
provider response bodies are never logged or returned.

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
