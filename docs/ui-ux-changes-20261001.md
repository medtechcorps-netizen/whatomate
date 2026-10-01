# UI/UX changes — 2026-10-01 batch

Branch: `claude/ui-ux-20261001`, cut from the pinned ui phase source
`6f25ea1919ee28856dee59d5fd121671214087e3` (tree `2a2c14e8`).

These changes ship only in the **next** release: after the current signed
release train finishes, the release session turns this branch into a new
reviewed ui phase-source child, re-pins it, rebaselines the contract and runs a
single-phase train. Nothing on this branch is deployed directly.

## Canary impact summary

Items below marked **canary-sensitive: yes** change something the production
canary driver exercises; the canary driver must be updated before the next
release for those. All other entries leave the canary hooks untouched.

| # | Change | Canary-sensitive |
|---|--------|------------------|
| 1 | Three-dots (overflow) menus open again | no |
| 2 | Pause AI no longer fails with "Agent not found" | no |
| 3 | Shared lead-flow helpers and dialogs (add lead, won/lost, invoice) | no |
| 4 | Customer workspace: lead-first "Next step" card | **yes - content only** (Customer workspace / revenue side rail) |
| 5 | Lead pipeline: follow-ups drawer, New lead dialog, explicit Won/Lost, invoice | no |
| 6 | Chat header "Next step" menu and hidden-number display | **yes - adjacent** (`chat-contact` row text for blank/placeholder numbers only) |
| 7 | Contacts, Follow-ups and Invoices pages: hidden numbers, links, plain copy | no |
| 8 | Inbox keeps following the newest message after a window resize | **yes** (message-pane scrolling) |

### What the canary driver owner must know (items 4 and 6)

- Unchanged: every canary data-testid (`omnichannel-*`, `chat-contact`,
  `chat-message`, `chat-message-list`), `#info-button`, `#notes-button`,
  `conversation-ai-toggle`, the navbar Inbox unread badge, message-pane
  scroll-to-bottom logic, `useMediaQuery('(min-width: 1280px)')`, the rail
  auto-open rules (ChannelsView `selectConversation`, ChatView session rule), the
  400px / 420px rail widths, the Sheet below 1280px, the rail root
  `data-testid="customer-revenue-workspace"` + `aria-label="Customer revenue
  workspace"`, the close button aria-label, the `Details` tab and
  `contact-details-panel`. No canary API path (`/api/me`, `/api/conversations*`,
  `/api/webhook`) is touched.
- Changed inside the rail (item 4): the Overview content and its height. The four
  KPI tiles, "New journey"/"Follow-up" buttons and the old section layout are
  replaced by a "Next step" card (`data-testid="lead-next-step"`,
  `data-state` none/open/won/lost) and compact sections; tab labels are larger
  (`text-xs`); the Copilot tab is hidden for users without copilot access. If the
  driver asserts any rail inner text ("Open journeys", "New journey", "Active
  journeys", "Care and bookings", "Packages and revenue") or measures the rail's
  own scroll height, it must be rebaselined. When the customer has an open or
  lost lead, the rail now also sends `GET /api/crm/pipelines` once per load (only
  for users who can read pipelines) to draw the stage stepper; strictly mocked
  specs that seed workspace `journeys` must mock that endpoint.
- Changed in ChatView (item 6): `chat-contact` rows and the chat header keep the
  exact `name || phone_number` text for every contact that has a name or a usable
  number (locked by a unit test). Only contacts whose number is blank or a
  WhatsApp-username placeholder (`bsuid:`…) now read "WhatsApp user" /
  "WhatsApp number hidden" instead of a blank or the raw placeholder. The
  three-dots trigger's accessible name changed from "Contact Options" to
  "Next step and more" / "More options" (new `data-testid="chat-more-options"`).
  The Call button is hidden for non-dialable placeholder numbers.

## Verification commands

Run from `frontend/`:

```
npm ci
npm run typecheck
npx --no-install vitest run src
npm run build
```

Batch verification for items 3-7 (2026-10-01): typecheck clean; vitest 41 files
/ 540 tests passed; build passed; `frontend/package.json` and
`package-lock.json` unchanged. The repo's fully mocked Playwright specs
(`e2e/tests/crm/crm-workflows-mocked.spec.ts`,
`e2e/tests/channels/mobile-workspace.spec.ts`,
`e2e/tests/settings/navigation-cleanup-mocked.spec.ts`) were run against the
local Vite dev server with a throwaway config that skips the backend-dependent
`globalSetup`: 35/35 passed. Visual checks used the Vite dev server against a
local synthetic mock API (no production calls), at 1440x900 in headless Edge
and in the built-in browser.

## Change log

### 1. Three-dots (overflow) menus open again

- **Owner asked:** "Clicking the three dots line shows nothing" (native chat
  header, screenshot 5).
- **Root cause:** the shared `DropdownMenu` wrapper declared `open?: boolean`
  with no default. Vue casts an absent boolean prop to `false`, and reka-ui only
  manages its own state when `open` is `undefined`, so every *uncontrolled*
  dropdown was pinned closed. The click emitted `update:open(true)` but nothing
  listened. Affected: the chat header menu, plus the Automation Studio and
  Automation Policies "More" menus. The pipeline card menu worked only because
  it is explicitly controlled.
- **Changed:** `frontend/src/components/ui/dropdown-menu/DropdownMenu.vue` now
  uses the same `useForwardPropsEmits` pattern as Popover/Dialog/Sheet, so only
  props a caller passes are forwarded. New
  `frontend/src/components/ui/dropdown-menu/DropdownMenu.test.ts` covers the
  uncontrolled and controlled modes. The uncontrolled case fails against the old
  wrapper and passes with the fix.
- **Side effect to QA:** Automation Studio / Automation Policies "More" menus now
  open; their destructive items still go through the existing confirm dialogs.
- **Verified:** vitest (new tests + full suite), typecheck, build.
- **Canary-sensitive:** no.

### 2. Pause AI no longer fails with "Agent not found"

- **Owner asked:** "Unable to pause AI" (screenshot 4: toast "AI replies were
  not paused / Agent not found").
- **Root cause (backend, confirmed by code trace):** `POST /api/chatbot/transfers`
  validates the requested `agent_id` with `findByIDAndOrg[models.User]`, i.e.
  `users.organization_id = <current org>`. That column is only the user's
  *home* organization; membership of other organizations lives in
  `user_organizations`. A user who works in a clinic organization other than
  their home one (clinic creator/reseller, org-switched member, super admin)
  passes the permission check but fails this lookup. The chat view always sent
  `agent_id = <me>`. A user marked "Away" gets the similar
  "Agent is currently away" rejection.
- **Changed (frontend-only mitigation):** `frontend/src/views/chat/ChatView.vue`
  `transferToAgent` still sends `agent_id = <me>` first; only when the backend
  answers exactly `404 Agent not found` or `400 Agent is currently away` (both
  rejected before any row is written) it retries once without `agent_id`. The
  pause is then an unassigned manual pause that the user still owns through
  `transferred_by` and can resume. The success toast says when the conversation
  went to the team handover queue or to the customer's assigned team member.
  Any other error (404 contact, 409, 403) is not retried.
- **Backend follow-up (not done here, needs owner decision):** make the agent
  lookups in `internal/handlers/agent_transfers.go` (create + assign),
  `teams.go` and `agent_analytics.go` membership-aware. No DB migration.
  Until then, cross-org users' pauses are queued rather than self-assigned
  (in orgs with SLA auto-close, a queued pause can auto-expire).
- **Tests:** `frontend/src/views/chat/ChatView.selection.test.ts` gains retry
  cases for both rejections and no-retry cases for 404 contact / 409 / 403; the
  existing self-assigned pause test is unchanged.
- **Verified:** vitest, typecheck, build.
- **Canary-sensitive:** no (`conversation-ai-toggle` and
  `/api/chatbot/transfers` are not canary hooks; chat-contact / chat-message /
  chat-message-list markup untouched).

### 3. Shared lead-flow helpers and dialogs

- **Owner asked:** a simple path from chat to pipeline, then invoice when won or
  follow-up when not closed (onboarding feedback; screenshots 1 and 2).
- **Changed (new, shared by the workspace and the pipeline page):**
  - `frontend/src/lib/crmFlow.ts` - pure helpers: active pipelines/stages,
    focus lead selection, lead state, invoice-for-lead (via
    `invoice.metadata.lead_id`), next follow-up, one follow-up urgency rule
    (overdue = before today, today = any time today), follow-up date presets,
    remembered pipeline per organization (localStorage, try/catch), default
    pipeline choice, lead/task payload builders, lost reasons.
  - `frontend/src/lib/contactAddress.ts` - how to show a contact's number or
    identity ("WhatsApp number hidden" for WhatsApp-username senders) and a
    display name that never shows a raw `bsuid:` placeholder.
  - `frontend/src/components/crm/LeadCreateDialog.vue` - "Add to pipeline":
    pipeline cards with description and stage chain, starting stage, lead
    title, optional value, optional follow-up (Tomorrow / In 3 days / Next week
    / pick a date), duplicate-lead notice, visible reason when the submit is
    disabled.
  - `frontend/src/components/crm/LeadOutcomeDialog.vue` - "Mark as won?" (with
    "Create the invoice next") and "Mark as lost" (reason chips + note).
  - `frontend/src/components/commerce/InvoiceQuickDialog.vue` - "Create
    invoice" for a customer/lead: custom amount prefilled from the lead, or a
    package sale; links the invoice to the lead in `metadata.lead_id`.
  - `frontend/src/services/productSuite.ts` - type additions only, plus an
    optional `reason` on `crmService.moveLead` (sent only when non-empty, so a
    plain move still sends exactly `{stage_id, version}`).
- **Tests:** `crmFlow.test.ts`, `contactAddress.test.ts`,
  `LeadCreateDialog.test.ts`, `LeadOutcomeDialog.test.ts`,
  `InvoiceQuickDialog.test.ts`.
- **Verified:** see batch verification above.
- **Canary-sensitive:** no.

### 4. Customer workspace: lead-first "Next step" card

- **Owner asked:** "From Omnichannel - there should be a clearer way to transfer
  the leads into our pipeline. Things in the workspace sometimes are not
  significant to show." (screenshot 1)
- **Changed:** `frontend/src/components/chat/CustomerRevenueWorkspace.vue`
  - A "Next step" card at the top of Overview drives the flow:
    - no lead: "Not in the pipeline yet" + one primary **Add to pipeline**
      button;
    - open lead: stage chips (click to move), **Won** / **Lost**, follow-up
      status (overdue / today / next date / "No follow-up scheduled") with
      "Set follow-up" and a "Done" tick for an overdue or undated follow-up;
    - won: **Create invoice** (or the linked invoice's number, total and
      paid/unpaid status with "Open invoices"); open follow-ups left on a won
      lead can be marked done;
    - lost: reason, **Reopen lead**, **New lead**.
  - The four KPI tiles (all zero for a new enquiry) became one muted money line
    shown only when something is non-zero; empty sections are one quiet line;
    "Invoices & packages" is hidden when empty; "Other leads" lists only leads
    besides the one in the card.
  - "Journey" wording replaced by "lead" everywhere; header shows "WhatsApp
    number hidden" with a visible explanation instead of a blank phone line, and
    no longer repeats the contact name in the channel badge; tab labels are
    larger; Copilot tab hidden without copilot access; pipeline load failures
    show an inline "Try again".
  - New optional props `channel`, `conversationId`, `requestedAction` and emit
    `action-consumed` (used by the chat "Next step" menu). Leads created from a
    WhatsApp conversation are now tagged source `whatsapp` and reference the
    conversation (`source_reference = conversation:<id>`).
  - Every new write (move, complete follow-up, invoice) reuses the existing
    canonical-contact mutation-context guard; a 409 refreshes the workspace.
- `frontend/src/components/chat/ContactInfoPanel.vue`: no developer-facing
  "No data configured" block when embedded in the rail; phone row uses the
  hidden-number display.
- `frontend/src/views/channels/ChannelsView.vue`: only `:channel` and
  `:conversation-id` added to the two workspace elements, and the screen-reader
  Sheet description text updated. Nothing else.
- **Tests:** new `CustomerRevenueWorkspace.test.ts`; `ContactBookingDialog.test.ts`
  updated (canonical-contact assertions kept).
- **Verified:** batch verification above, plus the full flow on the local mock
  (add lead with follow-up, move stage, mark won, create invoice).
- **Canary-sensitive:** **yes - content only.** See "What the canary driver
  owner must know" above. Rail open/close, breakpoints, widths, auto-open and
  test hooks are unchanged.

### 5. Lead pipeline: follow-ups drawer, New lead dialog, explicit Won/Lost

- **Owner asked:** "It is confusing on deciding the new lead pipeline here...
  the follow up trail is blocking the screen." (screenshot 2)
- **Changed:** `frontend/src/views/crm/PipelineView.vue`,
  `frontend/src/components/crm/LeadEditDialog.vue`
  - The fixed 310px follow-up column is gone. A header **Follow-ups** button
    (red badge when any are overdue) opens a side drawer showing this
    pipeline's follow-ups by default ("All follow-ups" toggle), each with the
    customer, due date, a complete button and "Show lead". Columns are fluid,
    so all five stages (including Lost) fit on a laptop screen.
  - **New lead** opens the shared "Add to pipeline" dialog (customer, pipeline,
    starting stage, value, follow-up). The board pipeline is labelled, shows its
    description, is remembered per organization and can be deep-linked with
    `?pipeline=<id>&lead=<id>` (IDs only).
  - Previous/Next only move between open stages (compact arrows that keep their
    accessible names). Open cards have **Won**, **Lost** and **Follow-up**;
    dragging into Won/Lost asks the same question. Won offers **Create invoice**
    (then shows "Invoice created"); Lost records a reason, shown on the card and
    editable in "Edit lead" while lost.
  - Cards link the customer name to the chat, show a follow-up badge (overdue /
    today / date / "No follow-up"), and drop the "x% probability" line.
  - KPI strip: Open leads, Pipeline value, Won this month, Open follow-ups (with
    overdue count).
  - Fixed a real bug: completing a follow-up linked to a lead bumped the lead's
    version on the server, so the next move of that lead failed with 409; leads
    are now reloaded, and a 409 on completing refreshes instead of failing
    repeatedly.
- **Tests:** new `frontend/src/views/crm/PipelineView.test.ts`; new e2e case in
  `frontend/e2e/tests/crm/crm-workflows-mocked.spec.ts` (follow-ups drawer);
  all existing accessible names used by the mocked e2e suite kept.
- **Verified:** batch verification above (35/35 mocked e2e, including the
  keyboard stage move and permission cases).
- **Canary-sensitive:** no.

### 6. Chat header "Next step" menu and hidden-number display

- **Owner asked:** three-dots menu showed nothing (screenshot 5); simpler flow
  from chat into the pipeline.
- **Changed:** `frontend/src/views/chat/ChatView.vue`
  - The three-dots menu (fixed in item 1) is now a "Next step" menu: **Add to
    pipeline**, **Schedule follow-up**, **Book appointment** (each opens the
    customer workspace and starts that action), then Show/Hide customer
    workspace, Open contact record, Copy phone number (only for a real number),
    Assign to agent. Items are permission- and entitlement-gated.
  - Trigger named "Next step and more" / "More options" (aria-label + title).
    It deliberately does not use a Tooltip wrapper: nesting `TooltipTrigger`
    around `DropdownMenuTrigger` breaks the menu's popper anchor and leaves the
    open menu off-screen (found during the visual check).
  - Header and contact list show "WhatsApp user" / "WhatsApp number hidden" only
    where the old output was blank or a raw placeholder; contacts with a name or
    usable number render exactly as before. Call button hidden for
    non-dialable placeholders.
- **Tests:** `ChatView.selection.test.ts` gains menu gating, requested-action
  wiring, copy-phone, hidden-number, stale-flag and legacy-name cases.
- **Verified:** batch verification above, plus the menu checked open in the
  browser at 1440px and in the narrow pane.
- **Canary-sensitive:** **yes - adjacent.** `chat-contact` row text changes only
  for contacts with a blank or placeholder number; see the canary note above.

### 7. Contacts, Follow-ups and Invoices pages

- **Owner asked:** "The contact number were not saved/missing in the CRM - is it
  UI/UX issues?" (screenshot 3) and general simplification.
- **Answer:** not a UI bug and not masking. The number was never stored: the
  regular WhatsApp Cloud API inbound path stores an empty phone when Meta sends
  the sender only as a business-scoped user ID (WhatsApp username users). This
  needs a backend fix (see "Backend follow-ups" below). The UI now explains it.
- **Changed:**
  - `frontend/src/views/settings/ContactsView.vue`,
    `ContactDetailView.vue`: "WhatsApp number hidden" badge with an explanation
    (also for screen readers) instead of a blank; name fallback "WhatsApp user".
    Real numbers render exactly as before.
  - `frontend/src/views/crm/TasksView.vue`: each follow-up shows the customer and
    the lead with "Open chat" / "Open lead" links; plain copy ("Follow-ups",
    "Open", "Overdue" as a working filter); same overdue rule as the pipeline;
    accessible, single-submit complete buttons.
  - `frontend/src/views/commerce/CommerceView.vue`: `?tab=invoices` /
    `?tab=packages` deep links; jargon removed ("Revenue desk" -> "Invoices &
    payments", "Tenant outstanding" -> "Unpaid", no "minor units" /
    "idempotent" wording).
- **Tests:** new `TasksView.test.ts`, `ContactsView.test.ts`; `CommerceView.test.ts`
  extended.
- **Verified:** batch verification above.
- **Canary-sensitive:** no.

### 8. Inbox keeps following the newest message after a window resize

- **Owner asked:** approved the fix reported by the release session: when the
  window is resized while the omnichannel transcript holds several long
  messages, the pane stopped following the latest message.
- **Root cause:** on resize the transcript reflows; the browser's scroll
  anchoring adjusts `scrollTop` and fires a scroll event *before* the
  transcript `ResizeObserver` runs. `handleMessageViewportScroll` then measures
  the pane as "not near the bottom" and turns bottom-following off, so the
  observer no longer scrolls down (reproduced: 127 px above the bottom).
- **Changed:** `frontend/src/views/channels/ChannelsView.vue` - the
  `omnichannel-message-viewport` element gets `overflow-anchor: none` (Tailwind
  `[overflow-anchor:none]`). No script, threshold or data-testid change.
  The native chat (`ChatView`, reka ScrollArea viewport) does not show the
  problem under the same test, so it is left unchanged.
- **Tests:** new mocked Playwright spec
  `frontend/e2e/tests/channels/message-pane-resize.spec.ts` (inbox and native
  chat: at the bottom at 1600px, narrow to 1300px, still within the 80px
  follow threshold; inbox also widened back). The inbox case fails without the
  fix (127 px) and passes with it (3/3 repeats); the chat case passes as a
  guard.
- **Canary-sensitive:** **yes - message-pane scrolling** (omnichannel inbox).
  The release session should re-verify the canary driver's resize / late-layout
  checks against this build.

## Backend follow-ups (not done on this branch - owner decision needed)

1. **Pause AI root cause** - make the agent lookups in
   `internal/handlers/agent_transfers.go` (create and assign), `teams.go` and
   `agent_analytics.go` membership-aware (`user_organizations`), keeping the
   reseller-membership validity check. No migration. Decide whether a
   self-pause should skip the "away" check.
2. **Contacts saved without a number (data integrity and privacy)** - the
   regular (non-Coexistence) Cloud API path creates contacts with
   `phone_number = ''` for username/BSUID-only senders. Because of the unique
   index on `(organization_id, phone_number)`, later username-only senders in
   the same organization can be attached to that same contact, and replies are
   addressed by its last-written BSUID, so a reply could reach a different
   customer. Fix: resolve such senders through the existing coexistence
   placeholder logic, stop `Recipient` sending `to` for placeholders, guard
   against empty phones, stop overwriting a different BSUID, and repair the
   existing empty-phone rows. Until then, avoid merging the empty-phone contact.
3. Optional product improvements: a real `lead_id` on invoices, pipeline
   rename/default/deactivate endpoints, pipeline stages in the workspace
   response, a closed-lead window for Won/Lost columns, an invoice PDF/share
   link for WhatsApp.
