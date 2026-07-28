# Meta Relay

The Meta relay is the durable, text-only Messenger and Instagram transport for
ReReply relay channel accounts. It is a separate process built from
`./cmd/meta-relay` and uses Redis as its acceptance, queue, and idempotency
boundary.

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

## Required environment

All deployments require:

| Variable | Meaning |
| --- | --- |
| `META_RELAY_REDIS_URL` | Redis URL. There is no in-memory durability fallback. |
| `META_RELAY_MESSENGER_APP_SECRET` | App secret for the Messenger parent app webhook route. |
| `META_RELAY_MESSENGER_VERIFY_TOKEN` | Verify token for the Messenger parent app callback. |
| `META_RELAY_INSTAGRAM_APP_SECRET` | App secret for the separate Instagram Login app webhook route. |
| `META_RELAY_INSTAGRAM_VERIFY_TOKEN` | Verify token for the Instagram Login app callback. |
| `META_RELAY_GRAPH_API_VERSION` | Explicit version such as `v25.0`; update deliberately when Meta support changes. |
| `META_RELAY_ACCOUNTS_JSON` | Account mappings. It contains environment-variable names, never secret values. |

The four webhook credential values must all be configured. The two app secrets
must differ, and the two verify tokens must differ.

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
