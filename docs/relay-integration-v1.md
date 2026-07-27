# ReReply signed-relay contract v1

Status: version 1, production contract

This contract connects Instagram, Messenger, email, web chat, or another approved
channel through a customer-operated HTTPS relay. The relay translates its
provider's events into ReReply's canonical envelopes and translates ReReply's
outbound envelopes back to the provider.

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

Do not parse and re-serialize JSON before verification. For `HEAD` health probes
and signed media `GET` requests, sign the empty byte sequence. Compare signatures
in constant time.

Inbound requests are signed by the relay with the inbound secret. Requests from
ReReply to the relay are signed with the outbound secret (or inbound-secret
fallback described above).

## Relay health probe

Before outbound approval, ReReply sends `HEAD` to `health_url` when configured,
otherwise to `relay_url`.

- HMAC header: signature of an empty body.
- Accepted response: any `2xx`, or `405 Method Not Allowed`.
- A network failure or any other status leaves the account degraded or pending.

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
