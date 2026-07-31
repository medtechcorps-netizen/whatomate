# ReReply Product Suite Runbook

This document describes the production boundaries for ReReply's commercial,
CRM, booking, commerce, Qwen Copilot, and omnichannel modules.

## Release policy

The product suite is released through a protected `main` branch. A production
release requires:

1. Go tests, migration tests, PostgreSQL RLS tests, `go vet`, frontend type
   checking, and the frontend production build to pass.
2. A review of schema changes and a tested database backup or recovery
   checkpoint.
3. Provider credentials to be stored through encrypted application settings or
   the deployment secret store. Credentials must never be committed.
4. A staged tenant rollout before the features are enabled for all customers.

Rollback the application to the preceding immutable image if health or
cross-tenant checks fail. Database changes in this release are additive; leave
the new tables in place during an application rollback unless a separately
reviewed data migration says otherwise.

## Tenant boundary

Tenant-owned records carry `organization_id` and are protected by both:

- handler-level organization scoping and permission checks; and
- PostgreSQL Row-Level Security driven by the request tenant context.

Plans, plan prices, plan entitlements, and global workspace template versions
are control-plane records and are not exposed through a tenant transaction.
Subscriptions, onboarding, privacy, support, CRM, bookings, packages, invoices,
payments, Copilot runs, channel accounts, and inbox records are tenant data.

Incoming channel events do not select a tenant from client-provided
`organization_id`. They resolve an opaque channel-account UUID through the
restricted database routing function. Cross-tenant regression tests are a
mandatory release gate.

## Commercial and onboarding lifecycle

1. A platform superadmin creates or updates a plan and its entitlements.
2. Only a platform superadmin assigns or changes a manual subscription for an
   organization. Reseller owners and administrators have read-only portfolio
   and subscription visibility; they cannot list private assignable plans or
   mutate tenant licenses.
3. The organization completes its profile and applies a versioned Clinic,
   Pharmacy, Wellness, or General workspace template.
4. Entitlements, rather than UI visibility alone, govern access to licensed
   functionality.
5. Billing and payment provider events are recorded idempotently before they
   change subscription state.

Tenant feature licensing applies to platform administrators as well as tenant
users. Assign a valid subscription or trial before setup and testing; do not
use an administrator identity to bypass expired or unlicensed features.

Manual licensing and manual payment recording are explicit administrative
actions. They do not claim that money was collected by a payment provider.

### Initial activation

The production migration applies a versioned, idempotent initial backfill for
the approved first-party workspace catalog:

| Plan | Code | Monthly MYR minor units | Licensed modules |
| --- | --- | ---: | --- |
| ReReply Starter | `rereply-starter` | `30000` | Core WhatsApp chat, chatbot, campaigns, templates and flows; commercial module entitlements remain disabled |
| ReReply Growth | `rereply-growth` | `60000` | Omnichannel, CRM, bookings, commerce and reviewed Qwen Copilot |

Sprint is advertised at RM900/month as a non-assignable coming-soon plan until
calling is enforced by the commercial entitlement layer. Enterprise is a
contact-sales offer and is not represented by a dummy zero-value subscription
price. Threads public engagement remains disabled in the published plans.

Published prices are immutable. Use a new unique price code whenever an
approved amount changes; never overwrite a price already referenced by a
subscription. The legacy RM0 Growth pilot price remains resolvable for existing
subscription identities but is marked `assignable=false`, so it cannot be used
for a new manual plan change. Moving an existing pilot workspace to RM600
requires an explicit audited assignment and never happens automatically.
After the initial catalog version is recorded, application restarts do not
reactivate archived plans or disabled prices and do not overwrite later
control-plane entitlement edits.

Reseller-specific plans may still be created through
`POST /api/admin/product/plans`. They must use unique plan and price codes.

As a platform superadmin, load the target-specific assignable catalog through
`GET /api/admin/organizations/{organization_id}/product/plans`, select the
exact plan and price IDs, and assign the plan through
`PUT /api/admin/organizations/{organization_id}/subscription`. The approval or
contract reference is mandatory:

```json
{
  "plan_id": "00000000-0000-0000-0000-000000000001",
  "plan_price_id": "00000000-0000-0000-0000-000000000002",
  "status": "trialing",
  "trial_days": 14,
  "manual_reference": "approved-pilot-reference"
}
```

Reseller owners and administrators may inspect
`GET /api/admin/organizations/{organization_id}/subscription` for organizations
in their own portfolio, but a reseller identity must receive `403 Forbidden`
from both the private assignable-catalog endpoint and the subscription mutation
endpoint.

After assignment, sign in as a normal tenant administrator and verify
`GET /api/product/entitlements` before applying a workspace template. An
expired, canceled, paused, incomplete, missing, or past-due subscription
outside an explicit unexpired grace window must not unlock a feature.

### Partner portfolio performance follow-up

The Partner Console currently obtains each organization's safe subscription
summary from the target-scoped admin endpoint, so a portfolio refresh performs
one subscription request per organization. Do not fold tenant subscription
rows into the existing reseller usage query without an explicit control-plane
authorization and RLS review. A future bounded, paginated bulk license-summary
endpoint should replace this request pattern before very large portfolios are
supported; track portfolio request count and latency until that endpoint is
available.

## Privacy and support

Retention policies are tenant-configurable by data category. Consent is stored
as an append-only event stream with a current state projection. Data-subject
requests have a verified, audited status workflow.

In this release, retention policies are a registry and operating schedule, not
an automated deletion or anonymization engine. Access, portability, correction,
restriction, and erasure fulfillment is performed through an approved external
or manual process. Record the evidence in the request resolution before marking
the request complete. Do not represent a configured retention period as proof
that customer data has been purged; add a reviewed execution worker and
deletion-evidence records before making that claim.

Support cases do not grant tenant access. Temporary support access requires a
separate, time-limited grant. Recovery checkpoints describe known restore
points; they are not proof that a restore succeeded.

## CRM, booking, packages, and payments

Lead stage changes, follow-up tasks, bookings, credit consumption, invoices,
and payment state changes use optimistic versions or database locks where
concurrent updates could lose data. Invoice totals are calculated by the
server in minor currency units.

Package credits use an append-only ledger. Correct a credit error with a new
ledger entry rather than editing historical entries.

## Qwen Copilot

Copilot is human-in-the-loop:

- it uses the configured Qwen model and tenant AI context;
- it can draft, summarize, qualify, or suggest actions;
- it persists the grounded run and safety warnings for review; and
- it never sends a message, creates a booking, or collects payment by itself.

A user must review and perform the downstream action. Feedback may link a
separately sent message to the Copilot run for audit purposes.

`expires_at` on a Copilot run is the operator's purge deadline in this release;
it does not itself delete the record. Schedule and evidence a tenant-scoped
purge job before representing Copilot retention as automatic.

The DashScope API key is encrypted at rest with the application encryption key.
Rotate any key that has appeared in chat, logs, screenshots, or support
material.

The reviewed default model is `qwen3.7-plus`. For a production Singapore
workspace, set `WHATOMATE_AI__QWEN_BASE_URL` to the workspace-specific
`https://{workspace-id}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1`
endpoint. The API key and base URL must belong to the same Alibaba Cloud region.
The shared `dashscope-intl` endpoint remains a compatibility default, not the
recommended production endpoint.

## Omnichannel activation

The normalized inbox supports WhatsApp, Instagram, Messenger, email, web chat,
and future providers through signed adapters. “Supported by the data model” is
not the same as “live in production.”

Legacy Meta WhatsApp is mirrored into the normalized inbox without changing its
delivery path. The migration creates a credential-free `meta_legacy` shadow
account and links each existing `messages` row to a stable conversation derived
from the legacy account and contact UUIDs. It does not create a second message,
channel credential, or outbox job. The migration is tenant-scoped, processes
bounded batches, selects only unlinked rows, and is safe to rerun.

New inbound messages, established WhatsApp sends, mobile-app echoes, campaign
sends, and read-state changes are mirrored as they occur. A mirror failure does
not retry or duplicate Meta delivery; rerunning the migration repairs the
missing link. Mirrored WhatsApp threads are read-only in the normalized
composer and reply through `/chat/{contactId}`, where the established
templates, media, and service-window controls remain authoritative.

Backfill only links a historical message when its tenant and
`whats_app_account` name match a current legacy account. If an account was
renamed and historical rows still carry an older name, reconcile that mapping
under an approved tenant-specific migration instead of guessing across
accounts.

Manual normalized-inbox sends are restricted to `service` and `transactional`
purposes. Marketing is rejected in this release. If a provider service window
is closed, delivery requires an approved business-initiation template. A
verified webhook for a tenant without a durable omnichannel entitlement is
accepted without domain dispatch, and its raw headers and payload are
discarded after an audit marker is recorded.

For every channel account:

1. Implement and verify the
   [ReReply signed-relay contract v1](./relay-integration-v1.md), including its
   absolute webhook URL, exact-body HMAC, envelopes, idempotency, and retries.
2. Complete provider ownership verification and any required app review.
3. Create a least-privilege credential and save it through the encrypted
   channel-account flow.
4. Configure the provider webhook to the account-specific public relay route.
5. Verify inbound signatures, event deduplication, retry behavior, and outbound
   status callbacks.
6. Run an inbound and outbound test with a non-customer test identity.
7. Confirm consent, quiet hours, opt-out behavior, and provider messaging
   windows.
8. Enable the account for one pilot organization and monitor delivery failures.

### Threads public engagement beta

Threads is a separate `threads` channel and is fail-closed behind both
`omnichannel.enabled` and `threads.public_engagement.enabled`. It accepts only
the signed `relay` provider. The feature is for public replies and mentions,
not messaging:

- every inbound conversation must carry a provider-owned public target in
  `external_conversation_id`;
- `metadata.engagement_type` must be exactly `reply` or `mention`;
- every outbound agent response must set `reply_to_external_id` to that same
  selected public target; and
- the account cannot be the default outbound channel or initiate a standalone
  post.

**Threads direct messages are not supported.** Do not label this channel as a
DM inbox, and do not translate a public reply into a private message. Threads
may offer messaging in its consumer product, but this ReReply integration does
not implement a Threads DM endpoint or permission.

Use least-privilege Threads OAuth access for a future approved relay:

- `threads_basic` for the connected Threads identity;
- `threads_read_replies` to read replies to the connected account's posts;
- `threads_manage_replies` to manage eligible public replies;
- `threads_content_publish` to create the public reply container used with
  `reply_to_id`; and
- `threads_manage_mentions` to retrieve and manage mentions.

Meta's official Threads API workspace documents the
[authorization scopes](https://www.postman.com/meta/threads/folder/34203612-e0373e84-de6b-46f1-b90d-3fea76ba6782),
[reply-management surface](https://www.postman.com/meta/threads/folder/34203612-1cc7918f-d63d-45fd-bdaa-e8855d0338cb),
[public reply request](https://www.postman.com/meta/threads/request/34203612-7abb4419-a216-4e06-8027-f9f4fcf8bee9),
and [`/me/mentions` request](https://www.postman.com/meta/threads/request/34203612-fc3f21da-0a53-44ab-80e2-8cd8c376a42a).
Verify the granted token scopes in Meta's access-token debugger before any
provider test.

This release does not contain an approved concrete Threads relay adapter.
Creating an entitled connection records a pending beta account, while **Test
and activation deliberately fail closed**. Do not override the pending status
or enable outbound delivery. Ship and review a Threads-specific adapter,
provider-scope validation, sandbox evidence, and end-to-end reply-target tests
before changing this gate.

TikTok remains disabled until the selected TikTok API product and business
account have been approved for the intended messaging use case.

## Operational checks

Monitor at minimum:

- `/health` and `/ready`;
- database and cache reachability;
- the worker heartbeat enforced by `/ready` (90-second lease);
- webhook signature failures and duplicate event rates;
- channel outbox age, attempts, and dead-letter counts;
- booking capacity conflicts;
- failed payment events and invoice balance mismatches;
- privacy request due dates;
- Copilot provider errors, latency, and safety warnings; and
- RLS verification at startup and in the release pipeline.

Alert on sustained failures rather than individual provider retries. Provider
webhooks commonly deliver duplicates and retry transient failures.
