# ReReply Visual Automation Policy Builder

The Automation Studio turns ReReply's durable customer lifecycle events into
visible, reviewable operating policies. It is deliberately separate from a
chatbot flow:

- a chatbot flow controls one live conversation;
- an automation policy reacts to customer events over hours, days, or weeks.

The first release is task-first. A policy may create accountable internal work,
but it cannot send a customer message, call a webhook, charge a payment method,
or mutate a booking, package, invoice, consent record, or CRM stage.

## Policy graph

Every policy uses the same small graph contract:

```json
{
  "nodes": [
    {
      "id": "booking-status",
      "type": "trigger",
      "config": {
        "event_types": [
          "booking.status_changed"
        ]
      }
    },
    {
      "id": "is-no-show",
      "type": "condition",
      "config": {
        "field": "event.metadata.to_status",
        "operator": "equals",
        "value": "no_show"
      }
    },
    {
      "id": "recovery-delay",
      "type": "delay",
      "config": {
        "minutes": 30
      }
    },
    {
      "id": "create-recovery-task",
      "type": "create_task",
      "config": {
        "title": "Recover no-show booking",
        "priority": "high",
        "due_in_minutes": 60,
        "owner": "contact_owner"
      }
    }
  ],
  "edges": [
    {
      "source": "booking-status",
      "target": "is-no-show",
      "branch": "always"
    },
    {
      "source": "is-no-show",
      "target": "recovery-delay",
      "branch": "true"
    },
    {
      "source": "recovery-delay",
      "target": "create-recovery-task",
      "branch": "always"
    }
  ]
}
```

V1 supports four node types:

| Node | Purpose |
| --- | --- |
| Trigger | Starts a run from one typed `CustomerActivityEvent`. |
| Condition | Compares an approved event field using a bounded operator set and follows one explicit true/false edge. |
| Delay | Schedules the next step durably; it does not hold an application process open. |
| Create task | Creates an idempotent `FollowUpTask` for the event's canonical contact. |

Activation rejects unknown node types or fields, duplicate node IDs, dangling
edges, cycles, unreachable nodes, multiple triggers, duplicate true/false
condition branches, invalid durations, and task actions without the required
fields. A condition may omit one branch; that outcome ends the run cleanly
without an action. Drafts may be incomplete while an administrator is editing
them.

## Starter policies

Automation Studio includes four editable starter policies:

1. **Post-visit care** — after a completed booking, wait one day and create a
   normal-priority check-in task.
2. **No-show recovery** — after a no-show, create a high-priority recovery task
   after a short operational delay.
3. **Package risk** — when a balance is low or a package is expiring, create a
   renewal-review task.
4. **Overdue invoice** — when an invoice becomes overdue, create a collections
   review task.

Templates create drafts. Nothing runs until an authorized user reviews and
activates the policy.

## Draft, activation, and run lifecycle

- Editing changes the mutable draft and increments its optimistic-concurrency
  version.
- Activation validates the complete graph and publishes an immutable version
  with its checksum, publisher, and publication time.
- The activation ledger is append-only at the database layer. An open interval
  may be closed once; published intervals cannot be retimed, reopened, or
  deleted.
- Every run is pinned to the immutable version that matched its trigger.
  Editing or reactivating a policy cannot change work already in progress.
- Pausing prevents new events from starting runs. Existing delayed runs keep
  their pinned definition and remain visible in the run monitor.
- Preview accepts synthetic example data only. It never loads a stored activity
  or contact, and it does not write a policy version, execution, step, task,
  scheduled job, activity event, or outbox row.
- Preview reports trigger overlap with active policies before the confirmation
  dialog. Activation checks the same condition again inside the serialized
  publish transaction. Behaviorally identical active policies are rejected.

## Durable and idempotent execution

The existing lifecycle outbox has one leased consumer and is not used as a
competing automation queue. A database trigger creates a tenant-scoped
automation receipt in the same transaction as every activity event. The
automation processor leases those receipts and fans matching events out into
version-pinned executions and scheduled steps. A run is unique for the
combination of organization, policy, and customer activity event.

Each step uses a deterministic identity derived from:

```text
organization + policy version + activity event + node
```

Retries therefore resume the same logical step. A task action also carries that
identity into `FollowUpTask.idempotency_key`, so a crash after task creation
cannot create a second task on replay. Leases, bounded retries, safe error
indicators, and step timestamps remain visible in the run monitor.

Automation-created tasks retain server-protected policy, version, execution,
source-event, and node lineage. Their resulting `task.created` activity carries
authoritative internal-only outbox provenance, so it is recorded for the
timeline without being delivered to configured external webhooks.

## Access and tenant boundaries

- `crm.automations:read` controls policy and version visibility.
- Run history additionally requires `contacts:read` and `tasks:read`, and its
  step response excludes rendered task output.
- `crm.automations:write` controls draft creation and editing.
- `crm.automations:execute` controls preview, activation, and pause.
- `crm.automations:delete` controls removal of eligible policies.
- The `crm.enabled` entitlement gates every policy endpoint and runtime action.
- Every read, mutation, uniqueness constraint, and background claim is scoped
  by `organization_id`; all new organization-owned tables are covered by
  PostgreSQL row-level security.
- Runtime-created tasks resolve the canonical contact inside the tenant
  transaction and retain policy, version, run, source-event, and node lineage.

## Operational rollout

Before activation in production:

1. Apply the new tables, indexes, integrity constraints, permissions, and RLS
   policies.
2. Give automation permissions only to the intended administrator or manager
   roles.
3. Create each starter policy as a draft and verify its preview path with a
   representative lifecycle event.
4. Start one automation processor and observe execution latency, retry count,
   failed steps, duplicate suppression, and task lineage.
5. Activate one low-volume policy, then scale the processor after lease
   recovery and tenant fairness are confirmed.

Customer messaging is a later action type. It must not be enabled until policy
evaluation includes current consent, channel eligibility, quiet hours,
frequency caps, duplicate-delivery suppression, approval gates, and a complete
delivery audit trail.
