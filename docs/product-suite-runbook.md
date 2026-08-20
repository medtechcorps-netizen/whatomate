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
`GET /api/admin/organizations/{target_organization_id}/product/plans`, select the
exact plan and price IDs, and assign the plan through
`PUT /api/admin/organizations/{target_organization_id}/subscription`. The approval or
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
`GET /api/admin/organizations/{target_organization_id}/subscription` for organizations
in their own portfolio, but a reseller identity must receive `403 Forbidden`
from both the private assignable-catalog endpoint and the subscription mutation
endpoint.

After assignment, sign in as a normal tenant administrator and verify
`GET /api/product/entitlements` before applying a workspace template. An
expired, canceled, paused, incomplete, missing, or past-due subscription
outside an explicit unexpired grace window must not unlock a feature.

### Partner portfolio performance

`GET /api/resellers/{id}/usage` returns each authorized organization's safe
subscription summary as a bounded page. `page` defaults to 1, `limit` defaults
to 50 and is capped at 100, and ordering is stable by organization name and ID.
The response includes `page`, `limit`, and `total`; `organization_count` and the
user, account, contact, and message totals continue to describe the full
authorized portfolio. Subscription summaries are resolved only for the
organizations returned on the current page, inside each organization's tenant
context. The Partner Console consumes those summaries directly, exposes
accessible page controls, and performs bounded page discovery for a deep-linked
workspace. It must not reintroduce one browser request per organization.

Aggregate usage still requires one RLS-scoped database statement per tenant.
That preserves isolation but leaves aggregate latency proportional to portfolio
size. Load-test the endpoint at the approved reseller ceiling and introduce
tenant-maintained snapshots or rollups before supporting portfolios where that
cost exceeds the latency budget.

## Privacy and support

Retention policies are tenant-configurable by data category. Consent is stored
as an append-only event stream with a current state projection. Data-subject
requests have a verified, audited status workflow.

General retention policies are still a registry and operating schedule, not a
complete deletion or anonymization engine. Access, portability, correction,
restriction, and erasure fulfillment is performed through an approved external
or manual process. Record the evidence in the request resolution before marking
the request complete. Copilot artifacts are the exception: their dedicated
tenant-scoped processor permanently removes expired runs and linked feedback.
Do not represent any other configured retention period as proof that customer
data has been purged until that resource has a reviewed execution worker and
deletion-evidence records.

Support cases do not grant tenant access. Temporary support access requires a
separate, time-limited grant. Recovery checkpoints describe known restore
points; they are not proof that a restore succeeded.

## CRM, booking, packages, and payments

Lead stage changes, follow-up tasks, bookings, credit consumption, invoices,
and payment state changes use optimistic versions or database locks where
concurrent updates could lose data. Invoice totals are calculated by the
server in minor currency units.

Lead archival is an explicit reversible lifecycle transition, not a delete or
an arbitrary status edit. `PUT /api/crm/leads/{id}/archive` and
`PUT /api/crm/leads/{id}/reopen` require the version the operator reviewed and
may carry a reason plus an idempotency key. Archive/reopen commits the lead,
customer activity, webhook outbox event, and audit record together. Archived
leads cannot be edited or moved until reopened; reopen derives the status from
the current stage and preserves historical won/lost timestamps. The active
board requests `include_archived=false`, while its archived view requests the
explicit `archived` status and exposes the reversible action.

Recurring availability and time off are tenant-scoped booking settings under
`/api/booking/resources/{resource_id}/availability-rules` and
`/api/booking/resources/{resource_id}/time-off`. Mutations are versioned and
audited. Active weekly windows use the booking resource's validated IANA
timezone, optional effective dates are inclusive resource-local calendar
dates, and overlapping active windows are rejected. Once any active rule
exists for a resource, a scheduled event must fit wholly inside one effective
window. Multi-day, overnight, nonexistent, ambiguous, or DST-offset-crossing
wall-clock schedules are rejected rather than guessed. Ambiguity checks derive
candidates from the IANA zone's actual nearby offsets, including fractional and
historical multi-hour rollbacks. A resource with no active rules remains
unrestricted for backward compatibility.

Time-off ranges are absolute instants. They cannot overlap another time-off
range or an existing non-cancelled event, and event create/update continues to
enforce time off independently of whether recurring hours are configured.

The chatbot flow palette exposes the backend-supported `set_variable` and
`ai_response` nodes. Variable assignments are typed and validated; duplicate
or empty names block saving. AI response nodes store the canonical optional
`prompt_template` field, fall back to the workspace AI configuration, and show
only a non-executing rendered preview in the builder. This chatbot graph is
separate from the task-first CRM Automation Studio authority boundary.

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

`expires_at` is enforced at both access and storage boundaries. Expired runs
are excluded from history, cannot accept feedback or be replayed through an
idempotency key, and are permanently removed with linked feedback by the
tenant-scoped Copilot retention processor. The processor runs immediately at
startup and hourly thereafter in bounded, idempotent batches.

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

An active `omnichannel.enabled` entitlement unlocks the module; it does not
copy or create provider accounts. Every organization owns its channel rows,
OAuth grants, external account identifiers and encrypted credentials. Never
reuse another organization's access token, Page ID, mailbox, webhook secret or
Threads authorization.

### New-workspace channel gate

Treat workspace creation, commercial licensing and provider connection as
three separate states. A newly created workspace starts onboarding with
`profile_required` and no placeholder provider or channel rows. After the
workspace profile and intended channels are saved, it advances through the
license and tenant-specific authorization gates.

| Channel | Tenant record required | External/runtime requirement |
| --- | --- | --- |
| WhatsApp | Active WhatsApp account and its `meta_legacy` inbox mirror | WABA ownership, access token and webhook subscription |
| Instagram | Active managed `instagram` + `relay` account created by Instagram Login | Dedicated managed Instagram app, encrypted dynamic registry binding, exact subscription/health/approval evidence; static mapping only for explicit off-pilot legacy accounts |
| Messenger | Active managed `messenger` + `relay` account created by Facebook Login for Business | Dedicated managed Messenger app, encrypted dynamic registry binding, exact Page/subscription/health/approval evidence; static mapping only for explicit legacy accounts |
| Threads | Active `threads` channel account created by workspace OAuth | Dedicated Threads app, entitlement, reviewed scopes and workspace authorization |
| Email/web chat | Tested signed-relay channel account | Matching mailbox/site relay and HMAC secrets |

`GET /api/integrations` reports WhatsApp, Instagram and Messenger separately in
`channel_connections`. The aggregate Meta count is inventory only and must
never be used as proof that every Meta channel is connected.

Instagram/Messenger relay identities and OAuth-managed Threads profile IDs have
one global workspace owner. The database migration installs
`uq_channel_accounts_global_routable_identity`; a historical duplicate fails
migration for explicit operator reconciliation instead of choosing a tenant or
copying credentials automatically.

Before go-live for every future organization:

1. Assign the intended plan to the exact target organization and verify the
   effective `omnichannel.enabled` entitlement. If Threads is intended, also
   require `threads.public_engagement.enabled` before starting OAuth.
2. Record which channels the organization intends to use; do not infer them
   from another tenant. Save them under **Launchpad → Workspace profile**.
3. Complete the tenant row and external/runtime registration for each intended
   channel. Managed Instagram and Messenger must use their dedicated OAuth
   flows and encrypted registry; prove the exact Page/profile ID is absent from
   every legacy static relay mapping before onboarding.
4. Run the account health test and require an active connection for every
   intended channel. Launchpad and support health remain incomplete while any
   declared channel is missing, degraded, or untested.
5. Verify one inbound event and one permitted outbound response with a
   non-customer test identity, then review retry/dead-letter telemetry.
6. Complete Threads separately; Threads authorization never follows from a
   WhatsApp, Instagram or Messenger connection.

Launchpad enforces this policy: `license_assigned` requires a feature-permitting
subscription for the declared channels (including the additional Threads
entitlement when applicable), and `channel_connected` is complete only when the declared
WhatsApp account is active and every other declared channel is active,
outbound-approved, free of a recorded error, and has a health-check timestamp.
Go-live approval cannot bypass either inferred step.

Managed Instagram and Messenger use the encrypted dynamic tenant registry;
clinics never place their provider tokens or HMAC secrets in deployment
environment mappings. `META_RELAY_ACCOUNTS_JSON` is legacy/off-pilot fallback
only. A static entry for the same Page/profile shadows dynamic resolution, so
drain its legacy work, remove it from every replica, verify absence, and only
then begin managed OAuth. Threads remains on its dedicated/BYO OAuth lifecycle.

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

For every legacy/off-pilot signed-relay account (including static Instagram or
Messenger) and every email or web-chat adapter, use the checklist below.
Managed Instagram and Messenger use their server-owned OAuth control planes;
legacy WhatsApp uses its established account flow, and Threads uses the
dedicated/BYO OAuth lifecycle in the next section. Do not create any managed
identity as a generic relay account.

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
the OAuth-managed `threads` provider. A generic signed-relay Threads account is
rejected. The feature is for public replies and mentions, not messaging:

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

Use least-privilege Threads OAuth access for the reviewed dedicated app:

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

Threads onboarding is workspace-specific:

1. Assign `omnichannel.enabled`, then grant the reviewed
   `threads.public_engagement.enabled` support entitlement to the exact target
   organization.
2. Configure a dedicated Threads App ID, HTTPS redirect URI, encrypted App
   Secret, webhook verify token, reviewed scopes, and approved App Review
   status. A Threads App ID cannot be shared by ReReply workspaces.
3. Complete OAuth from that workspace. The callback exchanges and validates
   the short- and long-lived tokens, verifies scopes, discovers the profile,
   encrypts the credential, and only then persists an active `threads` account.
   A failed stage leaves no active account.
4. Run Test, verify a signed inbound reply or mention and one permitted public
   reply, and confirm that token/permission health is current before go-live.

OAuth failure logs may include only safe provider diagnostics (HTTP status,
Graph error code/type, request ID, and trace ID). Never log tokens, app secrets,
provider error messages, or raw response bodies.

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
