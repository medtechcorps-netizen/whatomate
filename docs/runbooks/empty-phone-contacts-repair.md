# Empty-phone contacts: audit and manual repair

> **Status: requires release-session review. Do not run automatically.**
> Nothing in this runbook is a migration. Run the audit (section 2) read-only
> first. Run the repair (section 4) by hand, one organization at a time, only
> after a reviewer has signed off on that organization's audit output.
>
> **Release gate:** run sections 2.1, 2.2 and 2.5 for every organization
> *before* the fix is deployed, keep the output with the release notes, and
> repair (section 4) every row that has two or more senders before the
> deploy. See section 1.1 for why.

## 1. Background

Before the fix that ships with this runbook, the regular WhatsApp Cloud API
inbound path (accounts with `whatsapp_accounts.is_smb = false`) created a
contact with `phone_number = ''` when Meta identified the sender only by a
business-scoped user ID (BSUID, `from_user_id`) and sent no `from`. This
happens for WhatsApp username users.

`contacts` is unique on `(organization_id, phone_number)`, so every later
BSUID-only sender in the same organization was attached to that same row:

- their messages were stored in the same thread;
- `contacts.profile_name` and `contacts.bs_uid` were overwritten by the latest
  sender, so replies from that thread went to whichever sender wrote last;
- continuation jobs and Meta replays for an earlier sender could then fail the
  sender-identity check, and `/api/webhook` answered them with 503.

After the fix, a BSUID-only sender gets its own contact whose phone is the
Coexistence placeholder `bsuid:<first 40 hex chars of sha256(BSUID)>`, with
the metadata keys `coexistence_phone_unavailable`,
`coexistence_phone_placeholder`, `coexistence_user_id` and
`coexistence_username`.

### 1.1 What the fix does to existing empty-phone rows

The fix does change existing `''` / `'+'` rows in some cases, so audit them
before the deploy:

- A BSUID-only message (no `from`) from the BSUID currently in the row's
  `bs_uid` still lands on that row, as before.
- A message that carries a phone (`from`, or `contacts[].wa_id` when `from` is
  missing) and the row's current BSUID upgrades the row to that real phone
  **only** when the row is proven to hold this one sender: nothing was merged
  into it, it has at least one inbound message, and every inbound message has
  a continuation job whose payload names that BSUID (the section 2.2
  evidence). The upgrade records `metadata.empty_phone_upgraded_from` (the
  original `''` or `'+'`) and `metadata.empty_phone_upgraded_at`. Such a row no
  longer matches `phone_number IN ('', '+')`; section 2.1 still lists it
  through those keys. If the phone already belongs to another contact, the
  row is instead reconciled: its `bs_uid` is cleared and
  `metadata.coexistence_reconciled_contact_id` names the phone contact, and
  the phone stays empty.
- When the row is **not** proven to hold one sender (two or more
  `from_user_id` values, inbound messages without a continuation job, or a
  contact merged into it), the message is refused: the admission rolls back
  and `/api/webhook` answers 503, as it did before the fix, and the log line
  "Legacy empty-phone contact needs manual repair" names the contact. Meta
  retries for a limited time only, so repair such rows before the deploy.
  The same applies when a phone contact was merged into an empty-phone row.

Rows that were already upgraded by an earlier build without this check are
found by section 2.5, which does not depend on `phone_number`.

A blank-phone campaign recipient or a manual "+" phone could also produce a
`''` or `'+'` contact. Those rows have no inbound continuation jobs; section
2.2 shows them with zero senders. Review them by hand; do not rename them.

## 2. Read-only audit

Run these queries in a read-only transaction
(`BEGIN TRANSACTION READ ONLY; ... ROLLBACK;`). With tenant RLS enabled, use
the reviewed operator role and set the tenant per organization as described
in `docs/tenant-rls-runbook.md`.

### 2.1 Affected contacts and their WhatsApp account type

```sql
SELECT c.organization_id,
       c.id                      AS contact_id,
       c.phone_number,
       c.bs_uid,
       c.whats_app_account,
       c.merged_into_id,
       c.deleted_at,
       c.created_at,
       wa.id                     AS whatsapp_account_id,
       wa.is_smb
  FROM contacts c
  LEFT JOIN whatsapp_accounts wa
    ON wa.organization_id = c.organization_id
   AND wa.name = c.whats_app_account
 WHERE c.phone_number IN ('', '+')
    OR c.metadata ? 'empty_phone_upgraded_from'
 ORDER BY c.organization_id, c.created_at;
```

A row returned only because of `empty_phone_upgraded_from` was upgraded after
the fix with proof of a single sender. Re-check it with 2.2 and 2.5; it needs
no repair unless 2.5 lists it.

Include soft-deleted rows (no `deleted_at` filter): a merged empty-phone alias
still matches empty-phone lookups. Only rows with `is_smb = false` are in scope
for the BSUID repair. Treat an SMB (Coexistence) row as a separate incident.

### 2.2 Distinct senders per affected contact

Each inbound message has one continuation job. Its payload keeps Meta's
original sender fields.

```sql
SELECT m.contact_id,
       j.payload -> 'message' ->> 'from_user_id'        AS from_user_id,
       j.payload -> 'message' ->> 'from_parent_user_id' AS from_parent_user_id,
       NULLIF(j.payload -> 'message' ->> 'from', '')    AS from_phone,
       count(*)                                         AS inbound_messages,
       min(m.created_at)                                AS first_seen,
       max(m.created_at)                                AS last_seen
  FROM messages m
  JOIN scheduled_jobs j
    ON j.organization_id = m.organization_id
   AND j.kind = 'inbound_message.continuation'
   AND j.aggregate_id = m.id
 WHERE m.organization_id = :organization_id
   AND m.contact_id = :contact_id
   AND m.direction = 'incoming'
 GROUP BY 1, 2, 3, 4
 ORDER BY first_seen;
```

Also count inbound messages that have no continuation job. These cannot be
attributed automatically and need a manual decision:

```sql
SELECT count(*)
  FROM messages m
 WHERE m.organization_id = :organization_id
   AND m.contact_id = :contact_id
   AND m.direction = 'incoming'
   AND NOT EXISTS (
         SELECT 1 FROM scheduled_jobs j
          WHERE j.organization_id = m.organization_id
            AND j.kind = 'inbound_message.continuation'
            AND j.aggregate_id = m.id);
```

### 2.3 Whether a sender already has its own contact

```sql
SELECT id, phone_number, bs_uid, merged_into_id, deleted_at
  FROM contacts
 WHERE organization_id = :organization_id
   AND bs_uid IN (:from_user_ids)
 ORDER BY created_at;
```

### 2.4 Rows that reference the contact

The split in section 4.3 must re-point more than messages. List every column
named `contact_id` and count its rows for the affected contact before you
plan the split:

```sql
SELECT table_schema, table_name
  FROM information_schema.columns
 WHERE column_name = 'contact_id'
   AND table_schema = 'public'
 ORDER BY table_name;
```

### 2.5 Contacts holding more than one sender (independent of phone)

This finds a mixed row even after its `phone_number` was changed, for example
by a build that upgraded empty-phone rows without the single-sender check.

```sql
SELECT m.organization_id,
       m.contact_id,
       c.phone_number,
       c.bs_uid,
       count(DISTINCT COALESCE(
         NULLIF(j.payload -> 'message' ->> 'from_user_id', ''),
         NULLIF(j.payload -> 'message' ->> 'from_parent_user_id', ''))) AS senders
  FROM messages m
  JOIN scheduled_jobs j
    ON j.organization_id = m.organization_id
   AND j.kind = 'inbound_message.continuation'
   AND j.aggregate_id = m.id
  JOIN contacts c ON c.id = m.contact_id
 WHERE m.direction = 'incoming'
 GROUP BY 1, 2, 3, 4
HAVING count(DISTINCT COALESCE(
         NULLIF(j.payload -> 'message' ->> 'from_user_id', ''),
         NULLIF(j.payload -> 'message' ->> 'from_parent_user_id', ''))) > 1
 ORDER BY 1, 2;
```

A contact with a real phone can legitimately show two values when Meta sent
both a parent and a child BSUID for one person; check `from_phone` in 2.2. Any
other row in this list is in scope for the split in 4.3 (use the same steps;
the 4.2 rename does not apply to a row whose phone is already real).

## 3. Decide per contact

| Audit result | Action |
| --- | --- |
| 0 senders (no continuation jobs) | Not a BSUID row. Manual review only. |
| Exactly 1 `from_user_id`, no other contact holds it | Rename (4.2). |
| Exactly 1 `from_user_id`, another contact already holds it | Manual merge review. Do not rename: the placeholder would collide. |
| 2 or more `from_user_id` values | Split (4.3), then rename the remainder (4.2). Must be done before the deploy (release gate). |
| `bs_uid` empty and `coexistence_reconciled_contact_id` set | Reconciled after the fix. The BSUID now lives on the named phone contact. Split remaining senders (4.3); move this sender's messages to the named contact instead of renaming. |
| Any row with `is_smb = true` | Out of scope; escalate. |

Until a contact is repaired, check that only one BSUID has written to it
before an agent replies from that thread, and do not merge it into another
contact.

## 4. Manual repair (one organization per transaction)

### 4.1 Lock order (mandatory)

Changing `contacts.phone_number`, `bs_uid`, `merged_into_id` or `deleted_at`
fires the contact selector fence trigger. The trigger takes
`organizations ... FOR UPDATE NOWAIT` and a *try* advisory lock, and fails with
SQLSTATE `55P03` ("contact selector admission is busy") when inbound admission
holds them. Take the same locks first, in the order used by
`database.LockOrganizationPolicyScope`, so the trigger's locks are already
yours:

```sql
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
-- With tenant RLS enabled, also set the tenant context for :organization_id
-- as described in docs/tenant-rls-runbook.md.

-- 1. Contact selector / policy fence (blocks until inbound admission ends).
SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(
         'rereply:whatsapp_identity_review_selector:v1:' || :organization_id::text, 0));

-- 2. Organization row.
SELECT id FROM organizations
 WHERE id = :organization_id AND deleted_at IS NULL
 FOR UPDATE;

-- 3. Only now lock contact rows, in id order.
SELECT id FROM contacts
 WHERE organization_id = :organization_id AND id IN (:contact_ids)
 ORDER BY id
 FOR UPDATE;
```

If any statement fails with `55P03` or a lock timeout, `ROLLBACK` and retry the
whole transaction later. Never retry a single statement inside a failed
transaction. Inbound webhooks for the organization wait while you hold these
locks, so keep the transaction short and prepare every statement in advance.

### 4.2 Rename to the Coexistence placeholder (exactly one BSUID)

```sql
UPDATE contacts
   SET phone_number = 'bsuid:' || left(encode(sha256(convert_to(:from_user_id, 'UTF8')), 'hex'), 40),
       bs_uid       = :from_user_id,
       metadata     = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
                        'coexistence_user_id', :from_user_id,
                        'coexistence_phone_unavailable', true,
                        'coexistence_phone_placeholder', true,
                        'empty_phone_repaired_at', now()::text),
       updated_at   = now()
 WHERE organization_id = :organization_id
   AND id = :contact_id
   AND phone_number IN ('', '+');
-- Expect exactly 1 row. Otherwise ROLLBACK.
```

The placeholder must equal `coexistenceIdentityPlaceholder` in
`internal/handlers/whatsapp_coexistence_webhook.go` (prefix `bsuid:`, first 40
hex characters of SHA-256 of the BSUID). Check one value against the
application before you commit. If the sender used only a parent BSUID, use
`from_parent_user_id` for both the hash and `bs_uid`.

### 4.3 Split by `from_user_id` (two or more BSUIDs)

Keep the original row for the BSUID that is currently in `contacts.bs_uid`.
Replies from the thread are addressed to it today. For every other
`from_user_id`:

1. If section 2.3 found an existing contact for that BSUID, use it as the
   target. Otherwise create one:

   ```sql
   INSERT INTO contacts (id, organization_id, phone_number, bs_uid, profile_name,
                         whats_app_account, metadata, created_at, updated_at)
   VALUES (gen_random_uuid(), :organization_id,
           'bsuid:' || left(encode(sha256(convert_to(:other_user_id, 'UTF8')), 'hex'), 40),
           :other_user_id, :profile_name_from_payload, :whatsapp_account_name,
           jsonb_build_object('coexistence_user_id', :other_user_id,
                              'coexistence_phone_unavailable', true,
                              'coexistence_phone_placeholder', true,
                              'empty_phone_split_from', :contact_id::text),
           now(), now())
   RETURNING id;
   ```

   Take `profile_name_from_payload` from that sender's continuation payloads
   (`payload ->> 'profile_name'`), not from the shared row.

2. Move that sender's inbound messages, selected only through their
   continuation payloads:

   ```sql
   UPDATE messages m
      SET contact_id = :target_contact_id, updated_at = now()
     FROM scheduled_jobs j
    WHERE m.organization_id = :organization_id
      AND m.contact_id = :contact_id
      AND m.direction = 'incoming'
      AND j.organization_id = m.organization_id
      AND j.kind = 'inbound_message.continuation'
      AND j.aggregate_id = m.id
      AND j.payload -> 'message' ->> 'from_user_id' = :other_user_id;
   ```

3. Re-point the other references to those messages (customer activity
   events, inbox conversations and so on, from section 2.4) only where each
   row can be tied to a moved message. Leave outgoing messages, invoices,
   journeys and anything else that cannot be attributed on the original row,
   and list them for the clinic to review. Outgoing messages went to whichever
   BSUID was current at send time, which the database does not record.

After every other sender is moved, rename the original row with 4.2 using
the BSUID it kept.

### 4.4 Verify, then commit

```sql
-- No empty-phone row left for the repaired contacts.
SELECT id FROM contacts
 WHERE organization_id = :organization_id AND id IN (:contact_ids)
   AND phone_number IN ('', '+');

-- Each BSUID has exactly one live contact.
SELECT bs_uid, count(*) FROM contacts
 WHERE organization_id = :organization_id AND bs_uid IN (:from_user_ids)
   AND deleted_at IS NULL
 GROUP BY bs_uid HAVING count(*) <> 1;

-- Every moved inbound message now sits on the contact that holds its sender.
SELECT m.id
  FROM messages m
  JOIN scheduled_jobs j
    ON j.organization_id = m.organization_id
   AND j.kind = 'inbound_message.continuation'
   AND j.aggregate_id = m.id
  JOIN contacts c ON c.id = m.contact_id
 WHERE m.organization_id = :organization_id
   AND m.contact_id IN (:all_involved_contact_ids)
   AND COALESCE(NULLIF(j.payload -> 'message' ->> 'from_user_id', ''),
                j.payload -> 'message' ->> 'from_parent_user_id') <> c.bs_uid;
```

All three queries must return no rows. Then `COMMIT`; otherwise `ROLLBACK`.

## 5. After the repair

- Ask an agent to open each repaired thread and confirm that the history
  belongs to one customer.
- Pending continuation jobs or Meta replays for moved messages are checked
  against the contact that now holds their BSUID, so they stop failing the
  sender-identity check.
- Messages that were refused with 503 for this row (section 1.1) are accepted
  on Meta's next retry once the row is renamed to its placeholder.
- Record the organization, the contact IDs and the reviewer in the release
  notes for the session.
