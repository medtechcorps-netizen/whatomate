# Gmail Relay

The Gmail relay connects one Gmail or Google Workspace mailbox to ReReply's
signed-relay email channel. It polls Gmail history for new inbox messages,
normalizes them into ReReply conversations, and sends agent replies into the
original Gmail thread. Redis/Valkey is the durability boundary for OAuth state,
the encrypted refresh token, the Gmail history cursor, inbound work, and
outbound idempotency.

The first production mailbox is `realignphysiolates@gmail.com`.

## Production addresses

The DigitalOcean App Platform service is named `gmail-relay`. It listens on
port `8082` and is exposed below the `/gmail-relay` ingress prefix with that
prefix removed before the request reaches the service.

| Purpose | Address |
| --- | --- |
| Google OAuth redirect URI | `https://app.rereply.app/gmail-relay/oauth/google/callback` |
| Operator OAuth disconnect | `https://app.rereply.app/gmail-relay/oauth/google/disconnect` |
| ReReply relay URL | `https://app.rereply.app/gmail-relay/v1/accounts/email/realignphysiolates%40gmail.com` |
| Process liveness | `https://app.rereply.app/gmail-relay/livez` |
| Service readiness | `https://app.rereply.app/gmail-relay/readyz` |

The redirect URI must match the Google OAuth client exactly. Do not append a
slash, query string, or fragment. The `%40` in the relay URL is the URL-encoded
`@`; the relay still validates the decoded external account ID against the
configured mailbox.

## How the integration works

1. The operator authorizes the exact configured Google mailbox using OAuth
   2.0 authorization code flow, PKCE S256, offline access, and explicit
   consent.
2. The relay encrypts the refresh token before storing it in Redis. Access
   tokens are short-lived and are not persisted.
3. Every 30 seconds by default, the relay reads Gmail history, ignores sent,
   draft, spam, and trash messages, and durably queues new inbox messages.
4. A worker signs each canonical event and posts it to the connection-specific
   ReReply webhook. Gmail's thread ID becomes the stable ReReply conversation
   ID.
5. ReReply signs agent replies and posts them to the account relay URL. The
   relay validates the thread and recipient, constructs standard reply headers,
   and sends through the Gmail API. Provider lookup plus Redis idempotency
   prevents a retry from blindly sending a second copy.

Both directions use `X-ReReply-Signature-256: sha256=<hex-hmac>`, calculated
over the exact raw request body. Signed `HEAD` health checks use the HMAC of an
empty byte sequence. See [Relay Integration v1](relay-integration-v1.md) for
the complete envelope contract.

## Google Cloud setup

Use the `rereply-integrations` Google Cloud project:

1. Enable the Gmail API.
2. Configure the OAuth consent screen as **External**.
3. Add only these scopes:
   - `https://www.googleapis.com/auth/gmail.readonly`
   - `https://www.googleapis.com/auth/gmail.send`
4. While the app is in **Testing**, add
   `realignphysiolates@gmail.com` as a test user.
5. Create an OAuth client of type **Web application** named
   `ReReply Gmail Relay`.
6. Add the exact authorized redirect URI
   `https://app.rereply.app/gmail-relay/oauth/google/callback`.
7. Store the client ID and client secret directly in DigitalOcean encrypted
   runtime environment variables. Never commit or paste either credential into
   this repository.

The relay requests exactly the two listed scopes and verifies
`users.getProfile` after the callback. Authorization is rejected if Google
returns any mailbox other than `realignphysiolates@gmail.com`.

### Testing mode expires after seven days

Google documents that refresh tokens issued to an External OAuth app whose
publishing status is **Testing** expire after seven days unless the app requests
only basic identity scopes. This relay requests Gmail scopes, so the mailbox
must be authorized again approximately every seven days while Testing remains
enabled.

Moving the consent screen to **In production** removes that Testing-mode token
lifetime, but does not bypass Google's review rules. `gmail.readonly` is a
restricted scope and `gmail.send` is sensitive. Complete Google's OAuth app
verification—and any restricted-scope security assessment Google requires for
the deployment—before treating the integration as a permanent production
connection.

Official references:

- [OAuth for server-side web apps](https://developers.google.com/identity/protocols/oauth2/web-server)
- [Gmail API scopes](https://developers.google.com/workspace/gmail/api/auth/scopes)
- [OAuth publishing status and test users](https://support.google.com/cloud/answer/15549945)
- [OAuth app verification requirements](https://support.google.com/cloud/answer/13463073)

## DigitalOcean service configuration

Create an App Platform web service with these settings:

| Setting | Value |
| --- | --- |
| Name | `gmail-relay` |
| Repository/branch | `medtechcorps-netizen/whatomate`, `main` |
| Dockerfile | `docker/gmail-relay.Dockerfile` |
| HTTP port | `8082` |
| Health check | `/readyz` |
| Public route | `/gmail-relay` with prefix stripping enabled |
| Region | Same region as ReReply and `omnitech-cache` |

The service is a separate process. Do not add database or RLS environment
variables to it. Reuse the managed `omnitech-cache` Valkey service through its
encrypted connection URL and use a dedicated key prefix.

### Required environment

| Variable | Production value or purpose |
| --- | --- |
| `GMAIL_RELAY_REDIS_URL` | `${omnitech-cache.DATABASE_URL}`; use `rediss://` outside local development. |
| `GMAIL_RELAY_MAILBOX` | `realignphysiolates@gmail.com` |
| `GMAIL_RELAY_GOOGLE_CLIENT_ID` | Web OAuth client ID from Google Cloud. |
| `GMAIL_RELAY_GOOGLE_CLIENT_SECRET` | Web OAuth client secret; encrypted runtime secret. |
| `GMAIL_RELAY_GOOGLE_REDIRECT_URL` | `https://app.rereply.app/gmail-relay/oauth/google/callback` |
| `GMAIL_RELAY_ENCRYPTION_KEY` | Independent random secret of at least 32 bytes used to encrypt the refresh token. |
| `GMAIL_RELAY_SETUP_KEY` | Independent random secret of at least 32 bytes protecting OAuth start. |

Generate the encryption and setup values independently with a cryptographically
secure secrets tool, for example `openssl rand -base64 48`. Mark every secret
as an encrypted runtime variable in DigitalOcean.

The following three values form the ReReply pairing. They may all be omitted
during the initial OAuth bootstrap, but must be configured together before
mail can flow:

| Variable | Purpose |
| --- | --- |
| `GMAIL_RELAY_REREPLY_WEBHOOK_URL` | Absolute inbound webhook URL returned when the ReReply email connection is created. |
| `GMAIL_RELAY_REREPLY_INBOUND_SECRET` | One-time secret returned by ReReply; the relay signs inbound events with it. |
| `GMAIL_RELAY_REREPLY_OUTBOUND_SECRET` | Separate secret configured in both ReReply and the relay; ReReply signs outbound requests with it. |

Use distinct inbound and outbound signing secrets in production. Each should
contain at least 32 random bytes.

### Optional environment

| Variable | Default | Notes |
| --- | --- | --- |
| `GMAIL_RELAY_LISTEN_ADDR` | `:8082` | HTTP listen address. |
| `GMAIL_RELAY_REDIS_PREFIX` | `rereply:gmail-relay:` | Dedicated Redis/Valkey key namespace. |
| `GMAIL_RELAY_GOOGLE_AUTH_URL` | Google OAuth authorization endpoint | Override only in tests. |
| `GMAIL_RELAY_GOOGLE_TOKEN_URL` | Google OAuth token endpoint | Override only in tests. |
| `GMAIL_RELAY_GMAIL_API_BASE_URL` | Gmail API v1 endpoint | Override only in tests. |
| `GMAIL_RELAY_HTTP_TIMEOUT` | `30s` | Gmail and OAuth request timeout. |
| `GMAIL_RELAY_GMAIL_POLL_INTERVAL` | `30s` | Maximum normal delay before polling again. |
| `GMAIL_RELAY_QUEUE_POLL_INTERVAL` | `500ms` | Inbound worker queue polling interval. |
| `GMAIL_RELAY_FORWARD_TIMEOUT` | `15s` | ReReply webhook attempt timeout. |
| `GMAIL_RELAY_PROCESSING_LEASE` | `60s` | Must exceed forward timeout by more than six seconds. |
| `GMAIL_RELAY_INBOUND_RETENTION` | `168h` | Inbound acceptance and completion retention. |
| `GMAIL_RELAY_OUTBOUND_RETENTION` | `168h` | Outbound idempotency retention. |
| `GMAIL_RELAY_MAX_ATTEMPTS` | `12` | ReReply forwarding attempt budget. |
| `GMAIL_RELAY_WORKER_CONCURRENCY` | `2` | Concurrent inbound jobs; maximum `32`. |

Go durations such as `30s`, `5m`, and `168h` are required for duration values.

## Bootstrap and OAuth authorization

Deploy the service first with the required Google, Redis, encryption, setup,
and mailbox variables. Leave all three ReReply pairing variables empty during
this first deployment.

Start OAuth from a trusted operator shell. Keep the setup key out of shell
history and shared logs:

```sh
curl --fail --silent --show-error \
  --request POST \
  --header "X-ReReply-Setup-Key: $GMAIL_RELAY_SETUP_KEY" \
  https://app.rereply.app/gmail-relay/oauth/google/start
```

The response contains an `authorization_url` that expires after ten minutes.
Open it in the browser, select `realignphysiolates@gmail.com`, review the exact
two Gmail permissions, and approve. A successful callback displays **Gmail
connected**. Neither the access token nor refresh token is returned to the
browser.

If the wrong Google account is selected, the callback fails without storing
its refresh token. Start a new flow; OAuth state is single-use.

### Disconnect the mailbox

An authorized operator can remove ReReply's locally stored Gmail credential
without exposing it or accepting a mailbox identifier from the request. Send an
empty `POST` with the same setup key used to start OAuth:

```sh
curl --fail --silent --show-error \
  --request POST \
  --header "X-ReReply-Setup-Key: $GMAIL_RELAY_SETUP_KEY" \
  https://app.rereply.app/gmail-relay/oauth/google/disconnect
```

A successful, idempotent request returns only:

```json
{"mailbox":"realignphysiolates@gmail.com","disconnected":true}
```

The relay derives the credential key from its configured mailbox, deletes that
mailbox's encrypted refresh token, and clears the process's cached access token.
Every replica checks the shared credential before reusing a process-local access
token, so the deletion is observed on its next Gmail operation. The endpoint
cannot select or delete another mailbox. Synchronization, the signed account
health check, and outbound delivery then fail closed until the operator
completes OAuth again. `/livez` and `/readyz` continue to report process and
Redis/Valkey health; they do not claim that Gmail remains authorized.

This endpoint removes ReReply's credential; it does not call Google's token
revocation endpoint, delete Gmail messages, or delete conversation history
already stored in ReReply. The Google Account owner can separately revoke the
ReReply grant from Google Account permissions. Follow the applicable ReReply
retention or verified deletion process for stored CRM conversation data.

## Pair the ReReply connection

In **Omnichannel Inbox → Channel Connections**, create the Email signed-relay
connection with:

| Field | Value |
| --- | --- |
| Channel | Email |
| External mailbox ID | `realignphysiolates@gmail.com` |
| Relay URL | `https://app.rereply.app/gmail-relay/v1/accounts/email/realignphysiolates%40gmail.com` |
| Outbound secret | A new independent random signing secret. |

Save the connection's one-time inbound secret and webhook URL immediately.
Add the webhook URL and both signing secrets to the three pairing environment
variables, then redeploy `gmail-relay`.

Run **Test** on the connection. The signed `HEAD` test succeeds only when:

- the request signature and mailbox path match;
- Redis/Valkey is available;
- Google can refresh the credential and returns the configured mailbox; and
- a successful Gmail synchronization is recent.

After the test succeeds, explicitly approve outbound delivery. Changing the
relay URL or outbound secret disables outbound again and requires a new test
and approval.

Finally, send a real email from a different address to
`realignphysiolates@gmail.com`, verify it appears once in the unified inbox,
and send one agent reply. Confirm in Gmail that the reply is in the original
thread and addressed to the original sender.

## Operations and incident handling

- `/livez` proves only that the process can serve HTTP.
- `/readyz` proves Redis/Valkey is reachable and is the App Platform health
  check.
- The account's signed `HEAD` route is the end-to-end credential and sync
  health check. Use it through ReReply's **Test** action rather than exposing a
  signing secret in ad-hoc monitors.
- Monitor structured logs for `gmail_sync`, `inbound_forward`, and
  `outbound_settlement` component failures. Logs deliberately omit email
  bodies, OAuth tokens, signing secrets, and provider response bodies.
- Gmail `429` and transient `5xx` responses are retried. ReReply webhook `429`
  and `5xx` responses are queued with backoff. Permanent failures and exhausted
  retries are retained as dead-letter state in the dedicated Redis namespace.
- Do not flush or evict the relay namespace: it contains the refresh token,
  history cursor, acceptance records, and delivery idempotency state.
- Back up or persist the managed Valkey service according to the production
  recovery policy. A non-durable cache is not supported.

If Google authorization is revoked or a Testing token expires, run the OAuth
start flow again for the exact mailbox. Reauthorization replaces the encrypted
refresh token. Changing `GMAIL_RELAY_ENCRYPTION_KEY` makes the existing token
unreadable, so coordinate an encryption-key rotation with immediate
reauthorization. Rotate signing secrets by updating both ends, redeploying,
running **Test**, and approving outbound again.

## Current limitations

- One mailbox is supported per relay deployment. Deploy a separately keyed and
  prefixed service for another mailbox.
- This adapter supports Gmail and Google Workspace through the Gmail API; it is
  not a generic IMAP/SMTP connector.
- Synchronization is polling-based, so new email can take approximately one
  poll interval to appear.
- Only inbox messages are imported. Sent, draft, spam, and trash messages are
  excluded.
- Inbound content is normalized to text and limited to 10,000 characters.
  Attachments are not downloaded or exposed; an attachment-only message is
  represented by a text placeholder.
- Agent replies support exactly one text part, up to 10,000 characters, inside
  an existing Gmail thread. New standalone threads, CC, templates, and media
  attachments are not supported.
- The relay validates the recipient against the Gmail thread before sending.
  A mismatched or missing participant is rejected instead of guessing.
- While the Google OAuth app remains External/Testing, authorization is
  temporary and must be renewed about every seven days.

## Local build

```sh
docker build \
  -f docker/gmail-relay.Dockerfile \
  -t rereply-gmail-relay .
docker run --rm --env-file gmail-relay.env -p 8082:8082 rereply-gmail-relay
```

The CI security job builds this image and scans both OS packages and Go library
content for high and critical vulnerabilities.
