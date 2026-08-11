# ReReply signed-relay contract v1

Status: version 1, production contract

This contract connects Instagram, Messenger, email, web chat, the pending
Threads public-engagement beta, or another approved channel through a
customer-operated HTTPS relay. The relay translates its provider's events into
ReReply's canonical envelopes and translates ReReply's outbound envelopes back
to the provider.

## Connection values

Creating a relay connection returns:

- an absolute inbound webhook URL:
  `https://<rereply-host>/api/webhooks/channels/<channel-account-id>`;
- a one-time inbound signing secret;
- the relay URL configured by the operator.

Store the inbound secret in a secrets manager. It is shown once. ReReply signs
requests to the relay with the separately rotatable outbound secret. Until a
separate outbound secret is configured, ReReply uses the inbound secret in both
directions.

Relay URLs must use public HTTPS. ReReply rejects loopback, private, link-local,
multicast, unspecified, and other non-public destinations. Production must not
depend on redirects or local-development exceptions.

## HMAC signing

Every signed request uses:

```text
X-ReReply-Signature-256: sha256=<lowercase hex HMAC-SHA256>
```

Calculate the HMAC over the exact raw HTTP body bytes:

```text
hex(HMAC-SHA256(secret, raw_body))
```

Meta relay traffic has a second, deployment-held proof in
`X-ReReply-Meta-Provider-Proof-256`. The relay reads
`META_RELAY_REREPLY_PROVIDER_PROOF_SECRET`; ReReply reads the same value from
`WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET`. The proof is domain-separated
for canonical inbound POST bodies and readiness-v2 HEAD mappings. Both runtime
environment entries must be DigitalOcean `SECRET` values of at least 32 UTF-8
bytes with no surrounding whitespace. Rotate them together; there is no
previous-key grace period. Never put this value in account JSON, tenant config,
the channel API, logs, or `META_RELAY_PREFLIGHT_SECRETS_JSON`.

Both runtimes also emit
`X-ReReply-Meta-Provider-Proof-Key-ID`, a domain-separated HMAC fingerprint of
that key, on their readiness responses. Deployment preflight compares ReReply
`/ready` with each relay account probe so a one-sided rotation fails without
exporting or logging the secret.

An old/new mismatch fails readiness and causes ReReply to reject inbound Meta
events. The relay retains those jobs under its configured retry/dead-letter
policy; do not enable outbound or AI until both runtime services use the new
key.

CI sees only opaque secret declarations and cannot compare their bytes. After
every rotation, run the proof-backed ReReply Test for every mapped account, then
require a fresh external-customer text DM and administrator approval before
delivery is re-enabled.

Do not parse and re-serialize JSON before verification. For `HEAD` health probes
and signed media `GET` requests, sign the empty byte sequence. Compare signatures
in constant time.

Inbound requests are signed by the relay with the inbound secret. Requests from
ReReply to the relay are signed with the outbound secret (or inbound-secret
fallback described above).

For Meta relay mappings, every account-scoped inbound and outbound HMAC value
must contain at least 32 UTF-8 bytes with no surrounding whitespace and be
unique across both directions and all accounts.

Each Meta mapping's `access_token_env`, `rereply_inbound_secret_env`, and
`rereply_outbound_secret_env` references must resolve to exactly one non-empty
DigitalOcean `SECRET` on the relay service. The structural preflight validates
presence and type before app-spec mutation without reading or logging the
values. The relay's Redis URL, app secrets, verify tokens, and deployment-held
provider proof are subject to the same `SECRET` requirement.

## Relay health probe

Before outbound approval, ReReply sends `HEAD` to `health_url` when configured,
otherwise to `relay_url`.

- HMAC header: signature of an empty body.
- Accepted response: any `2xx`, or `405 Method Not Allowed`.
- A network failure or any other status leaves the account degraded or pending.

Messenger and Instagram have a stricter production profile. A `2xx` response
must include all of these exact headers; `405` is not accepted for those two
channels:

```text
X-ReReply-Relay-Readiness: v2
X-ReReply-Channel: messenger|instagram
X-ReReply-External-Account-ID: <exact provider asset ID>
X-ReReply-Channel-Account-ID: <exact ReReply channel account UUID>
X-ReReply-Organization-ID: <exact ReReply organization UUID>
X-ReReply-Meta-Business-ID: <numeric reviewed asset-owner Business Portfolio ID>
X-ReReply-Meta-Provider-Proof-256: sha256=<deployment-provider-proof>
```

The Meta relay emits them only after verifying Redis, the token-to-asset Graph
binding, the exact app's per-asset `messages` webhook subscription, the exact
ReReply organization/channel-account mapping, and separate verified Tech
Provider and approved App Review gates. ReReply compares the organization UUID
as well as the channel-account, provider asset, and numeric Meta Business tuple
against its deployment-held inventory. Deployment preflight proves that runtime
copy matches the protected GitHub ownership inventory. Deploy the relay and
strict ReReply validator together as a compatible release, then require the
signed-header account preflight to pass before Test or outbound approval.

Messenger runtime delivery requires Advanced Access for `pages_messaging` and
`pages_manage_metadata`. The protected production inventory has a stricter
future-organization profile: it also requires `pages_show_list`,
`pages_read_engagement`, and `business_management`. The deployment workflow
runs that structural comparison before changing DigitalOcean, then retains the
signed live account probes after the release is ready.

ReReply accepts Meta readiness only from its deployment-held
`WHATOMATE_META_RELAY__BASE_URL` and resolves the expected organization, asset,
channel-account, and Meta Business tuple from
`WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON`. The relay independently pins
the ReReply origin with `META_RELAY_REREPLY_BASE_URL`. Tenant-provided relay or
health URLs cannot replace either production trust anchor.

For a Meta ChannelAccount, `relay_url` must be exactly
`{WHATOMATE_META_RELAY__BASE_URL}/v1/accounts/{channel}/{external_account_id}`.
`health_url` must be absent or exactly the same URL. ReReply rejects any other
host, path, channel, or asset before sending the signed health request.

Changing the relay URL or outbound secret automatically disables outbound
delivery and returns the account to pending. Test it again, then explicitly
approve outbound delivery.

## Relay to ReReply: inbound webhook

Send `POST` to the exact absolute webhook URL with `Content-Type:
application/json` and a valid inbound signature. Maximum body size is 2 MiB.

Envelope:

```json
{
  "external_account_id": "provider-account-123",
  "events": [
    {
      "dedupe_key": "provider-event-987",
      "provider_event_id": "provider-event-987",
      "type": "message",
      "occurred_at": "2026-07-27T01:15:00Z",
      "message": {
        "external_message_id": "message-456",
        "conversation": {
          "external_id": "thread-123",
          "subject": "Appointment enquiry"
        },
        "sender": {
          "external_id": "customer-42",
          "address": "customer@example.com",
          "display_name": "Alya",
          "role": "customer"
        },
        "direction": "incoming",
        "parts": [
          {
            "type": "text",
            "text": "Can I book Tuesday morning?"
          }
        ],
        "sent_at": "2026-07-27T01:14:58Z",
        "received_at": "2026-07-27T01:15:00Z"
      }
    }
  ]
}
```

Rules:

- `external_account_id` must exactly match the connection.
- `events` must contain at least one event.
- every `dedupe_key` is required, stable across retries, and unique within the
  request;
- timestamps use RFC 3339 UTC;
- supported event types are `message`, `message_status`, `read`, and `reaction`;
- a message requires a provider message ID, conversation external ID, sender
  external ID, and at least one valid part.

Message-status callback:

```json
{
  "dedupe_key": "status-message-456-delivered",
  "provider_event_id": "provider-status-321",
  "type": "message_status",
  "occurred_at": "2026-07-27T01:16:00Z",
  "message_status": {
    "external_message_id": "message-456",
    "type": "delivered",
    "occurred_at": "2026-07-27T01:16:00Z"
  }
}
```

Status `type` values should be one of ReReply's canonical message events:
`accepted`, `sent`, `delivered`, `read`, or `failed`. A failed status may include
`error_code` and `error_message`.

Read-receipt payload:

```json
{
  "dedupe_key": "read-thread-123-1700",
  "type": "read",
  "occurred_at": "2026-07-27T01:17:00Z",
  "read": {
    "conversation": { "external_id": "thread-123" },
    "external_message_ids": ["message-456"],
    "reader": { "external_id": "customer-42", "role": "customer" },
    "read_at": "2026-07-27T01:17:00Z"
  }
}
```

Reaction payload:

```json
{
  "dedupe_key": "reaction-message-456-customer-42",
  "type": "reaction",
  "occurred_at": "2026-07-27T01:18:00Z",
  "reaction": {
    "external_message_id": "message-456",
    "sender": { "external_id": "customer-42", "role": "customer" },
    "emoji": "👍",
    "removed": false,
    "occurred_at": "2026-07-27T01:18:00Z"
  }
}
```

Successful response:

```json
{
  "data": {
    "accepted": true,
    "duplicate": false,
    "retry": false,
    "event_count": 1
  }
}
```

The API envelope wrapper can add standard response metadata. A duplicate
delivery returns `accepted: true` and `duplicate: true`.

## ReReply to relay: outbound requests

ReReply sends signed `POST` requests to `relay_url`:

```json
{
  "type": "message",
  "channel": "email",
  "external_account_id": "provider-account-123",
  "data": {
    "organization_id": "4b3c81b0-264d-4f15-ad87-9f65f3a5f112",
    "message_id": "1db68cf2-88ee-43f3-a1c8-e71f3908922d",
    "idempotency_key": "agent-send-20260727-0001",
    "purpose": "service",
    "conversation": { "external_id": "thread-123" },
    "recipient": {
      "external_id": "customer-42",
      "address": "customer@example.com",
      "role": "customer"
    },
    "subject": "Appointment enquiry",
    "parts": [
      { "type": "text", "text": "Tuesday at 10:00 is available." }
    ]
  }
}
```

For `type: "message"`, return `2xx` JSON:

```json
{
  "provider_message_ids": ["message-789"],
  "external_conversation_id": "thread-123",
  "provider_request_id": "request-abc"
}
```

At least one non-empty `provider_message_ids` value is required. ReReply also
sends:

- `type: "read"` with `conversation` and `external_message_ids`;
- `type: "subscribe"` with `external_account_id` and `channel`.

Those operations may return any `2xx`; `204 No Content` is supported.

### Threads public-engagement profile

This profile is a forward contract for a future approved Threads relay. The
current release keeps Threads accounts pending and fails Test because no
concrete Threads relay adapter is installed.

For a public reply or mention, the inbound relay must set:

```json
{
  "conversation": {
    "external_id": "threads-public-reply-or-mention-id",
    "metadata": {
      "engagement_type": "reply"
    }
  }
}
```

`engagement_type` must be `reply` or `mention`. The stable
`conversation.external_id` is the public provider target. ReReply requires an
outbound `reply_to_external_id` that exactly matches the selected conversation
target. A future relay must map that target to Meta's public `reply_to_id`
operation and reject a missing or mismatched target.

**Threads direct messages and standalone posts are not supported by this
profile.** The relay must never reinterpret a ReReply contact or conversation
as a Threads DM recipient, and it must never publish an agent response without
the existing public reply/mention target.

The least-privilege public-engagement scopes are `threads_basic`,
`threads_read_replies`, `threads_manage_replies`, `threads_content_publish`,
and `threads_manage_mentions`. Confirm them against
[Meta's official Threads API workspace](https://www.postman.com/meta/threads/overview),
including the official
[reply-management](https://www.postman.com/meta/threads/folder/34203612-1cc7918f-d63d-45fd-bdaa-e8855d0338cb)
and
[mentions](https://www.postman.com/meta/threads/request/34203612-fc3f21da-0a53-44ab-80e2-8cd8c376a42a)
requests, during implementation and app review.

## Idempotency and retries

- Treat `data.idempotency_key` as a provider-side unique key and return the same
  provider message IDs for every replay.
- ReReply queues outbound work durably and retries retryable network, timeout,
  `429`, and `5xx` failures with backoff, up to the configured job attempt limit.
- A non-retryable error or exhausted retry budget is dead-lettered and exposed
  in Channel Connections and tenant health.
- For inbound delivery, retry the exact signed body after a non-`2xx` response.
  `503` can include `Retry-After: 5` while another worker holds the processing
  lease.
- Never change an inbound event's `dedupe_key` during a retry.

## Activation checklist

1. Create the connection and securely save the one-time inbound secret.
2. Configure the absolute ReReply webhook URL in the provider relay.
3. Implement exact-body HMAC verification in both directions.
4. Implement `HEAD`, outbound message responses, provider idempotency, and
   inbound status callbacks.
5. Run **Test** in Channel Connections.
6. Inspect warnings and send a provider sandbox event.
7. Explicitly approve outbound delivery.
8. Monitor failed/retrying outbox counts and tenant health.
