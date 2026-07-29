# ReReply Customer Revenue CRM

This upgrade turns ReReply's existing contact, inbox, pipeline, booking,
package, invoice, payment, and Copilot modules into one customer operating
system. It deliberately reuses the current domain records instead of creating
a second CRM beside them.

## Release 1 — Customer graph and lifecycle events

- `Contact` remains the canonical customer record.
- `ContactIdentity` remains the mapping from a customer to each WhatsApp,
  social, email, or web-chat identity.
- `CRMLead` represents a customer journey or opportunity. A customer may have
  more than one journey.
- `CustomerActivityEvent` is the append-only lifecycle stream used by the
  timeline, automations, and outbound webhooks.
- `GET /api/contacts/{id}/workspace` returns a permission-aware aggregate of
  identity, journeys, tasks, bookings, packages, invoices, payments, and
  timeline events.
- `POST /api/contacts/{id}/merge` previews duplicate-contact collisions by
  default. A confirmed merge requires an idempotency key and retains a
  traceable alias for the source profile.

Customer activity and its outbox event are written on the same database
transaction as the domain change. This prevents the UI timeline and downstream
automation from disagreeing about whether an event happened.

## Release 2 — Inbox-native workspace

The Customer Revenue Workspace is available from both the WhatsApp chat and
the provider-neutral omnichannel inbox. It keeps the current tag and chatbot
session metadata features, then adds:

- active journeys, stage, owner, value, and next action;
- follow-up tasks and quick task creation;
- upcoming and recent appointments;
- package balance and expiry risk;
- invoice and payment context;
- a unified lifecycle timeline;
- human-in-the-loop Copilot summary, qualification, and action extraction.

Copilot output is advisory. It never sends a message or changes a record
without a user action.

## Release 3 — Care continuity and revenue intelligence

Care-continuity automation consumes durable scheduled jobs and lifecycle
outbox events. The initial policies create internal, idempotent follow-up tasks
for:

- completed visits that need a post-visit check-in;
- no-shows and cancellations that need recovery or rebooking;
- low or expiring package balances;
- overdue invoices.

The processor does not automatically message customers. Teams can review and
act from the inbox, task queue, or dynamic CRM segment.

`GET /api/crm/insights` provides pipeline, attendance, collections, package,
and follow-up indicators. `GET /api/crm/segments` and
`GET /api/crm/segments/{key}/contacts` expose live operational cohorts.
The dashboard widget engine also accepts the CRM, booking, invoice, payment,
and package data sources.

## Follow-on — Visual automation policies

Automation Studio makes the care-continuity layer configurable without
creating another customer model. Administrators build a policy from typed
lifecycle-event triggers, explicit conditions, durable delays, and internal
task actions. Preview is read-only, activation publishes an immutable version,
and every run retains event-to-task lineage.

The first release remains task-first: it cannot send customer messages or
perform other external side effects. The graph contract, starter policies,
runtime guarantees, permissions, and rollout checks are documented in
[`visual-automation-policy-builder.md`](visual-automation-policy-builder.md).

## Safety and operational guarantees

- Every new record and query is organization scoped and included in tenant
  row-level security.
- Activity, automation, merge, and webhook operations are idempotent.
- Duplicate merges are preview-first, transactional, audited, and fail closed
  on unresolved uniqueness collisions.
- Contact-bound writers resolve and lock the canonical profile, revalidate
  assignment visibility, and retry safely if a merge changes the alias path.
- Meta message webhooks acknowledge regular inbound messages only after the
  contact, message, lifecycle activity, and outbox transaction commits.
- Post-acknowledgement media and chatbot work is resumed from a leased durable
  job. Every remote chatbot effect has a separately committed, tenant-scoped
  pre-attempt claim and a deterministic replay identity.
- Direct and campaign marketing sends resolve and lock the canonical contact,
  then recheck the latest opt-out state immediately before provider delivery.
  If a provider attempt has an uncertain result, ReReply stops it for manual
  reconciliation instead of risking an automatic duplicate.
- Transfer reads and mutations use explicit read, write, and pickup
  permissions; shared dashboard layouts remain owner-controlled.
- `CurrentConversationOnly` applies equally to stored messages and durable
  message activities. A session or transfer assignment provides the cutoff;
  without a safe boundary, message history is hidden.
- Financial and consent history is preserved rather than rewritten when its
  invariants make reassignment unsafe.
- Webhook credentials keep their existing values. The webhook form uses
  explicit field names and autocomplete boundaries so password managers do not
  place login credentials into URL or HMAC-secret fields.
- No credential, API key, signing secret, or provider token needs to be rotated
  for this release.

The product rationale and source comparison are documented in
[`competitive-benchmark.md`](competitive-benchmark.md).

## Verification

Before deployment:

1. Run the serial backend suite:
   `go test -p 1 ./internal/models ./internal/contactutil ./internal/channel
   ./internal/database ./internal/assignment ./internal/handlers
   ./internal/worker ./cmd/whatomate -count=1`.
2. Run `go vet` over the same backend packages and the focused race suite for
   inbound leases, RLS chatbot delivery, consent/merge delivery, and concurrent
   transfer assignment/resume.
3. Run frontend type checking, targeted ESLint, and the production build.
4. Run the webhook autofill and CRM workspace/Insights Playwright coverage.
5. Apply database migrations in staging and confirm the new RLS policy.
6. Start one processor instance, verify lease recovery, manual-review routing,
   and idempotent task creation, then scale out. `SKIP LOCKED` claims and lease
   heartbeats make concurrent replicas safe after that initial observation.

## Rollout sequence

1. **Foundation:** deploy the database integrity changes and backend APIs
   together. Confirm tenant policies, append-only activity enforcement, contact
   merge previews, and pending outbox depth before enabling the new UI.
2. **Workspace:** deploy the frontend and enable the customer workspace for an
   internal team first. Measure workspace load errors, task/journey creation
   failures, and time-to-next-action. Copilot remains review-only.
3. **Continuity and intelligence:** start one care-continuity processor,
   observe lease recovery and webhook retries, then scale horizontally.
   Enable Insights and the new widget sources only for roles with their
   underlying product permissions.

Recommended release health indicators are outbox age and failure rate,
duplicate-task suppression, merge collision rate, workspace p95 latency,
no-show recovery tasks completed, expiring-package follow-ups completed,
overdue amount recovered, pipeline conversion, and attendance rate.
