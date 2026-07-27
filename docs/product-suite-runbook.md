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

1. A platform owner creates or updates a plan and its entitlements.
2. A platform owner or reseller assigns a subscription to an organization.
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

Create the plan once through `POST /api/admin/product/plans`. Use a unique plan
and price code for each commercial catalog; never reuse an existing code to
change a published price.

```json
{
  "code": "omnitech-growth",
  "name": "Omnitech Growth",
  "description": "CRM, scheduling, commerce, reviewed AI and shared inbox",
  "vertical": "general",
  "status": "active",
  "trial_days": 14,
  "is_public": false,
  "prices": [
    {
      "code": "omnitech-growth-myr-month",
      "currency": "MYR",
      "unit_amount_minor": 0,
      "interval": "month",
      "interval_count": 1
    }
  ],
  "entitlements": [
    {"key": "crm.enabled", "value_type": "boolean", "value": true, "enforcement": "hard"},
    {"key": "bookings.enabled", "value_type": "boolean", "value": true, "enforcement": "hard"},
    {"key": "commerce.enabled", "value_type": "boolean", "value": true, "enforcement": "hard"},
    {"key": "copilot.enabled", "value_type": "boolean", "value": true, "enforcement": "hard"},
    {"key": "omnichannel.enabled", "value_type": "boolean", "value": true, "enforcement": "hard"}
  ]
}
```

The zero amount above is a safe trial-catalog placeholder, not a recommended
selling price. Set the approved amount before using the plan commercially.

Assign the plan to one tenant through
`PUT /api/admin/organizations/{organization_id}/subscription`:

```json
{
  "plan_code": "omnitech-growth",
  "price_code": "omnitech-growth-myr-month",
  "status": "trialing",
  "trial_days": 14,
  "manual_reference": "approved-pilot-reference"
}
```

After assignment, sign in as a normal tenant administrator and verify
`GET /api/product/entitlements` before applying a workspace template. An
expired, canceled, paused, past-due, incomplete, or missing subscription must
not unlock a feature.

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
