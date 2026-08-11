# Meta relay deployment preflight

`/readyz` proves only that the relay process and Redis durability boundary are
available. It does not prove that the intended organization, Meta Business,
Page, Instagram account, app owner, or ReReply ChannelAccount is bound. Every
production deployment therefore runs `scripts/meta_relay_preflight.py` twice:
`--validate-only` before the workflow's DigitalOcean spec update or
protected-main deployment, then signed live binding probes after service
readiness.

The temporary [staging-only Messenger review relay](meta-messenger-review-relay.md)
is outside this production transport. Production preflight rejects every
reserved review-runtime key by presence, including an apparently disabled or
empty declaration; remove the staging wiring instead of carrying it into a
production spec.

## Protected source and runtime copies

The gate compares the protected inventory with every deployment-held copy:

1. The protected GitHub `production` Environment contains a reviewed expected
   inventory. It pins both apps, their owning Meta Businesses, separate App
   Review and Tech Provider states, exact `live` app modes, the exact approved
   permission sets, the review evidence, and every account's ownership tuple.
2. The exported DigitalOcean app spec contains the relay app declarations,
   `META_RELAY_ACCOUNTS_JSON`, `META_RELAY_REREPLY_BASE_URL`, and the SECRET
   declaration `META_RELAY_REREPLY_PROVIDER_PROOF_SECRET`.
3. The same app spec's `omnitech-web` service contains
   `WHATOMATE_META_RELAY__BASE_URL` and
   `WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON`, plus the SECRET declaration
   `WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET`.

The script requires the ReReply inventory copy to equal the protected GitHub
document, every relay mapping to equal that inventory, and both runtime base
URLs to equal the approved production origins. Missing, extra, cross-wired, or
tenant-chosen bindings fail closed.

After that comparison, each account must pass a signed
`HEAD /v1/accounts/{channel}/{external_account_id}` probe. A bare `204` is not
enough. The response must attest all of these exact values:

| Header | Required value |
| --- | --- |
| `X-ReReply-Relay-Readiness` | `v2` |
| `X-ReReply-Channel` | Expected channel |
| `X-ReReply-External-Account-ID` | Expected Page/Instagram numeric ID |
| `X-ReReply-Channel-Account-ID` | Expected ReReply ChannelAccount UUID |
| `X-ReReply-Organization-ID` | Expected ReReply organization UUID |
| `X-ReReply-Meta-Business-ID` | Expected asset-owner Meta Business ID |
| `X-ReReply-Meta-Provider-Proof-256` | Structurally valid deployment-held provider-proof signature |
| `X-ReReply-Meta-Provider-Proof-Key-ID` | Domain-separated key fingerprint matching ReReply `/ready` |

The script never accepts a Meta access token, never prints signing secrets or
provider response bodies, never follows redirects, and never calls Meta
directly. The relay's provider checks use Graph `GET`; no mutating Meta API is
part of this gate. The deployment check verifies that both runtime services
declare the provider-proof key as a non-empty DigitalOcean `SECRET`. Because
DigitalOcean does not reveal secret bytes, each runtime derives a
domain-separated HMAC key fingerprint instead: ReReply exposes its copy on
`/ready`, and every successful relay account probe exposes the relay copy. The
monitor compares those fingerprints without receiving or printing the shared
secret. ReReply also cryptographically verifies the proof during Test and on
every inbound Meta event.

### Ownership limitation

A Page token or Instagram user token can prove the asset ID it represents and
whether the app is subscribed. Those token families cannot independently prove
which Business Portfolio owns the asset/app or whether that portfolio is the
verified Tech Provider. App Review approval and Tech Provider verification are
also different facts and must never be collapsed into one status.

Until a separately scoped, read-only business-management audit token is
available, an administrative reviewer must verify app ownership, asset-owner
Business IDs, App Review, and Tech Provider status in Meta. The reviewer,
timestamp, and durable evidence URL are mandatory in the protected inventory
and deployed app declarations. Do not infer ownership from a similarly named
business or from a Tech Provider screenshot belonging to a different portfolio.

Governance records are time-bounded. `reviewed_at` may be at most five minutes
ahead of the deployment clock and expires after 90 days. Refresh both deployed
app declarations and the protected inventory with a current review/evidence
record before that deadline.

### Messenger permission profiles

The relay process needs Advanced Access for these two permissions to deliver
Messenger traffic for an already provisioned Page:

- `pages_messaging`
- `pages_manage_metadata`

That runtime minimum is not the production onboarding standard. The protected
inventory must list Advanced Access for all five permissions below so the next
organization can be discovered and its Page/Business ownership reviewed:

- `pages_messaging`
- `pages_manage_metadata`
- `pages_show_list`
- `pages_read_engagement`
- `business_management`

If any Instagram mapping uses `facebook_login`, the Messenger parent app must
also list Advanced Access for `instagram_basic` and
`instagram_manage_messages`. A permission that is merely **Ready for testing**
does not satisfy `app_review_status: "approved"` and must not be put in the
protected approved-permission list.

## Production configuration

When `meta-relay` exists, the GitHub `production` Environment must define both
values below. A protected inventory with no deployed `meta-relay` service also
fails closed. Only the absence of both the service and inventory is treated as
"not provisioned".

### Expected inventory

Create the non-secret Environment variable
`META_RELAY_EXPECTED_ACCOUNTS_JSON`. Its global app records and every account
row are authoritative; `organization_name` is optional display text only.

All IDs in the following example are illustrative and do not identify a real
Meta app, Business Portfolio, Page, Instagram account, organization, or ReReply
ChannelAccount.

```json
{
  "messenger_app": {
    "app_id": "100000000000001",
    "app_mode": "live",
    "owner_business_id": "300000000000001",
    "app_review_status": "approved",
    "app_review_permissions": [
      "business_management",
      "pages_manage_metadata",
      "pages_messaging",
      "pages_read_engagement",
      "pages_show_list"
    ],
    "tech_provider_status": "verified",
    "review": {
      "reviewer": "Operations Reviewer <reviewer@example.com>",
      "reviewed_at": "2026-08-06T04:30:00Z",
      "evidence": "https://evidence.example.com/meta/messenger-review-2026-08-06"
    }
  },
  "instagram_app": {
    "app_id": "100000000000002",
    "app_mode": "live",
    "owner_business_id": "300000000000001",
    "app_review_status": "approved",
    "app_review_permissions": [
      "instagram_business_basic",
      "instagram_business_manage_messages"
    ],
    "tech_provider_status": "verified",
    "review": {
      "reviewer": "Operations Reviewer <reviewer@example.com>",
      "reviewed_at": "2026-08-06T04:30:00Z",
      "evidence": "https://evidence.example.com/meta/instagram-review-2026-08-06"
    }
  },
  "accounts": [
    {
      "key": "organization-messenger",
      "organization_id": "11111111-1111-4111-8111-111111111111",
      "organization_name": "Example organization",
      "meta_business_id": "200000000000001",
      "channel": "messenger",
      "external_account_id": "700000000000001",
      "rereply_account_id": "22222222-2222-4222-8222-222222222222",
      "access_token_env": "META_ORGANIZATION_MESSENGER_PAGE_TOKEN",
      "rereply_inbound_secret_env": "REREPLY_ORGANIZATION_MESSENGER_INBOUND_SECRET",
      "rereply_outbound_secret_env": "REREPLY_ORGANIZATION_MESSENGER_OUTBOUND_SECRET"
    },
    {
      "key": "organization-instagram",
      "organization_id": "11111111-1111-4111-8111-111111111111",
      "organization_name": "Example organization",
      "meta_business_id": "200000000000001",
      "channel": "instagram",
      "external_account_id": "800000000000001",
      "instagram_api_mode": "instagram_login",
      "rereply_account_id": "33333333-3333-4333-8333-333333333333",
      "access_token_env": "META_ORGANIZATION_INSTAGRAM_USER_TOKEN",
      "rereply_inbound_secret_env": "REREPLY_ORGANIZATION_INSTAGRAM_INBOUND_SECRET",
      "rereply_outbound_secret_env": "REREPLY_ORGANIZATION_INSTAGRAM_OUTBOUND_SECRET"
    }
  ]
}
```

Replace the illustrative Business and organization values with IDs verified in
the two protected systems. App IDs and Meta asset/Business IDs must be numeric;
ReReply organization and ChannelAccount IDs must be canonical UUIDs. A display
name, username, sender profile, or similarly named Page is not an identifier.
All mappings for one ReReply organization must use the same asset-owner Meta
Business ID.

Reviewer identity uses `Name <email>` format. `reviewed_at` must be RFC3339 UTC,
and `evidence` must be a credential-free HTTPS URL without a query or fragment.
The evidence should show the app ID, exact owner Business ID, `live` app mode,
App Review state, exact Advanced Access permission list, and Tech Provider
state; do not store tokens in the artifact or URL. Permission names must be
lowercase Meta identifiers with no duplicates. The Messenger list must contain
the five-item future-organization profile above. The deployed comma-separated list is
normalized as a set, then fails on any missing or unexpected permission.

### Deployed app declarations

The following must be inspectable `GENERAL` values in DigitalOcean and exactly
match the corresponding expected app record:

```text
META_RELAY_REREPLY_BASE_URL=https://app.rereply.app
META_RELAY_MESSENGER_APP_ID=<numeric-app-id>
META_RELAY_MESSENGER_APP_MODE=live
META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID=<numeric-business-id>
META_RELAY_MESSENGER_APP_REVIEW_STATUS=approved
META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS=business_management,pages_manage_metadata,pages_messaging,pages_read_engagement,pages_show_list
META_RELAY_MESSENGER_TECH_PROVIDER_STATUS=verified
META_RELAY_MESSENGER_REVIEWED_BY=Operations Reviewer <reviewer@example.com>
META_RELAY_MESSENGER_REVIEWED_AT=2026-08-06T04:30:00Z
META_RELAY_MESSENGER_REVIEW_EVIDENCE=https://evidence.example.com/meta/messenger-review-2026-08-06

META_RELAY_INSTAGRAM_APP_ID=<numeric-app-id>
META_RELAY_INSTAGRAM_APP_MODE=live
META_RELAY_INSTAGRAM_APP_OWNER_BUSINESS_ID=<numeric-business-id>
META_RELAY_INSTAGRAM_APP_REVIEW_STATUS=approved
META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS=instagram_business_basic,instagram_business_manage_messages
META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS=verified
META_RELAY_INSTAGRAM_REVIEWED_BY=Operations Reviewer <reviewer@example.com>
META_RELAY_INSTAGRAM_REVIEWED_AT=2026-08-06T04:30:00Z
META_RELAY_INSTAGRAM_REVIEW_EVIDENCE=https://evidence.example.com/meta/instagram-review-2026-08-06
```

Messenger and Instagram Login use separate apps. Their owner Business IDs may
be the same only when the review evidence proves that exact ownership. The two
review statuses remain independent even when one reviewer checked both apps.

### Relay runtime secret declarations

Before any DigitalOcean mutation, the candidate app spec must declare each of
these fixed `meta-relay` variables exactly once with `type: SECRET` and a
configured non-empty value:

- `META_RELAY_REDIS_URL`
- `META_RELAY_MESSENGER_APP_SECRET`
- `META_RELAY_MESSENGER_VERIFY_TOKEN`
- `META_RELAY_INSTAGRAM_APP_SECRET`
- `META_RELAY_INSTAGRAM_VERIFY_TOKEN`

For every `META_RELAY_ACCOUNTS_JSON` row, the variable named by each of these
fields must also exist exactly once as a non-empty `SECRET` on `meta-relay`:

- `access_token_env`
- `rereply_inbound_secret_env`
- `rereply_outbound_secret_env`

The structural gate checks only the declaration, type, and whether DigitalOcean
reports a configured value. It never compares, decrypts, or prints a token,
Redis credential, verify token, app secret, or account HMAC value. A missing,
duplicated, empty, or `GENERAL` declaration fails `--validate-only`, preventing
a candidate spec from being applied only to fail later at relay startup.

### ReReply runtime trust declarations

The `omnitech-web` service must also contain these inspectable `GENERAL` values:

```text
WHATOMATE_META_RELAY__BASE_URL=https://app.rereply.app/meta-relay
WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON=<exact protected inventory JSON>
```

The relay and ReReply services must additionally hold these two `SECRET`
declarations, containing the same deployment-managed value:

```text
meta-relay:   META_RELAY_REREPLY_PROVIDER_PROOF_SECRET
omnitech-web: WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET
```

The value must contain at least 32 UTF-8 bytes with no leading or trailing
whitespace. Do not put it in `META_RELAY_ACCOUNTS_JSON`, tenant configuration,
any API payload, or `META_RELAY_PREFLIGHT_SECRETS_JSON`. The structural CI gate
checks that each secret entry exists exactly once, is typed `SECRET`, and has a
configured value. Its live phase compares only the domain-separated
fingerprints emitted by the two runtimes; it never retrieves or prints the
values. Rotate both
runtime declarations together: this release has no previous-key grace period,
so a one-sided rotation fails closed. During a mismatch, readiness/Test fails
and ReReply rejects inbound Meta delivery. The relay keeps failed inbound jobs
under its normal retry/dead-letter policy; outbound and AI must remain disabled
until both services use the new key.

DigitalOcean exports secret entries opaquely. The preflight and scheduled
monitor therefore compare a domain-separated HMAC fingerprint from ReReply
`/ready` against the fingerprint on each relay account readiness response. A
one-sided secret rotation now fails before account Test. Immediately after any
provider-proof rotation, still run ReReply **Test** for every mapped account. Test
cryptographically verifies the readiness proof, clears delivery while it runs,
and writes server-only `meta_provider_proof_version: "v1"` only after success.
Then require the fresh external-customer text DM and administrator approval
again. Never treat a passing key-fingerprint monitor as a substitute for this
post-rotation Test.

The pre-deploy check parses the JSON and requires it to equal
`META_RELAY_EXPECTED_ACCOUNTS_JSON`. This is the runtime authority ReReply uses
to bind a ChannelAccount to the expected organization, provider asset, and Meta
Business ID. Each Meta ChannelAccount `relay_url` must equal
`{WHATOMATE_META_RELAY__BASE_URL}/v1/accounts/{channel}/{external_account_id}`;
`health_url` must be absent or equal that same URL. A tenant-editable URL is not
an authority source.

`META_RELAY_ACCOUNTS_JSON` must also include `organization_id` and
`meta_business_id` on each deployed account, alongside the normal relay fields
and exact `rereply_webhook_url`. `organization_name` is allowed only in the
protected expected inventory; the relay mapping rejects it and never uses it as
authority. Keep the deployed mapping as `GENERAL`: it contains IDs, URLs, and
names of secret environment variables, never the secret values.

### Account-scoped signing secrets

Create the Environment secret `META_RELAY_PREFLIGHT_SECRETS_JSON`. Its keys are
the expected `rereply_outbound_secret_env` names, and each value is the exact
account-scoped outbound HMAC secret already configured on the relay and the
matching ReReply ChannelAccount. Each value must contain at least 32 UTF-8
bytes. The preflight rejects shorter material without logging the value.

```json
{
  "REREPLY_ORGANIZATION_MESSENGER_OUTBOUND_SECRET": "<secret>",
  "REREPLY_ORGANIZATION_INSTAGRAM_OUTBOUND_SECRET": "<different-secret>"
}
```

Do not put Page/Instagram access tokens, app secrets, verify tokens, inbound
HMAC secrets, or the deployment provider-proof secret in this GitHub secret.
Every account-scoped inbound and outbound HMAC secret must be unique across
both directions and all accounts, contain at least 32 UTF-8 bytes, and have no
leading or trailing whitespace.

## Onboarding or reconnecting an organization

1. Independently review both Meta apps. Record their exact app-owner Business
   IDs, explicit `live` modes, App Review states, Advanced Access permission
   sets, and Tech Provider states with reviewer/time/evidence. For Messenger,
   require the five-item future-organization profile even though the running
   relay needs only two. Development or Ready-for-testing states never qualify.
2. Record the asset-owner `meta_business_id` and the exact numeric Page or
   Instagram professional-account ID. Current Page/IG tokens are insufficient
   evidence of Business ownership.
3. Create new ReReply ChannelAccounts under the exact `organization_id` and
   generate fresh account-scoped inbound/outbound HMAC secrets. Never repoint
   an old row by changing its external ID or organization.
4. Add the new ownership fields, mappings, and secret env values to
   a local candidate DigitalOcean app spec first. Add the same authoritative IDs
   and env names to the protected expected inventory; copy that full inventory
   into the ReReply service trust env; add only outbound HMAC values to the
   secret map. Keep both runtime base URLs pinned to the production values above.
   Declare both provider-proof variables as `SECRET`, never `GENERAL`, and never
   place their shared value in the inventory or preflight secret map. Also
   declare every fixed relay secret and every environment variable referenced by
   `access_token_env`, `rereply_inbound_secret_env`, and
   `rereply_outbound_secret_env` as a non-empty `SECRET` exactly once.
5. Run the structural preflight against that candidate spec and do not call
   `doctl apps update` or save the equivalent console change unless it passes.
   The GitHub workflow reads the already-applied DigitalOcean spec, so it cannot
   retroactively protect a preceding direct/manual spec mutation.
6. Remove stale active relay mappings after the rollback decision. Missing and
   unexpected mappings both fail the exact comparison.
7. Push the release. The workflow runs `--validate-only` again before its DigitalOcean
   spec/code deployment mutation; a mismatch stops the release. It then deploys
   and runs the signed v2 probes after service readiness. Both stages must pass.
8. Run **Test** for the new ReReply connection. Strictly after Test passes, send
   a new incoming **text** DM from a genuine external customer profile that is
   not listed as an admin, developer, or tester on either Meta app. Then refresh
   ReReply and obtain explicit administrator approval. An app-role profile can
   work in Development mode and is not production evidence. An older or
   replayed message, attachment-only photo, sticker, or reaction also does not
   count in the phase-one relay and must not be used as webhook proof.

Starting Test/Re-certify blocks new and queued outbound/AI delivery and clears
the prior proof marker. A provider request already dispatching across the
network may still complete; the control cannot revoke a request Meta has
already received.

For a structural comparison without live binding probes:

```sh
META_RELAY_EXPECTED_ACCOUNTS_JSON="$EXPECTED_JSON" \
  python3 scripts/meta_relay_preflight.py \
    --app-spec candidate-production-app-spec.json \
    --validate-only
```

To audit the currently deployed spec instead, pipe
`doctl apps spec get "$APP_ID" --format json` to the same command without
`--app-spec`.

The production workflow uses `--validate-only` only for its pre-deploy stage.
Do not replace the post-deploy signed probes with it: structural validation
cannot detect an expired token, wrong token asset, missing subscription, or an
old relay that lacks the v2 ownership attestation.

The read-only `Meta Relay Production Monitor` workflow runs the same structural
and signed account probes daily at 01:00 UTC and can also be started manually.
It uses the protected `production` Environment and makes no DigitalOcean or Meta
mutation. Treat a failure as an operations alert: token, subscription, mapping,
governance freshness, runtime trust, or provider-proof declaration drift may
otherwise remain hidden until the next deployment or customer message. Because
secret values remain opaque, the monitor cannot detect two present but unequal
provider-proof values; use the mandatory per-account Test after rotation.
