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

## Verification commands

Run from `frontend/`:

```
npm ci
npm run typecheck
npx --no-install vitest run src
npm run build
```

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
