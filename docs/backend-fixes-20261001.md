# Backend fixes — 2026-10-01

Branch: `claude/backend-fixes-20261001`, cut from the pinned ui phase source
`6f25ea1919ee28856dee59d5fd121671214087e3` (internal tree `a43572db`).
Backend only: no `frontend/`, `release/`, `.github/` or `docker/` changes, no
database migrations, no RLS/policy changes.

Owner-approved follow-ups from the UI/UX batch on `claude/ui-ux-20261001`
(see `docs/ui-ux-changes-20261001.md` on that branch). These reach production
only through the release process: the release session decides how to phase
them (suggested: their own backend phase, before the UI batch, because fix 2 is
a data-safety fix).

## 1. Pause AI: membership-aware agent lookup

- **Problem:** `POST /api/chatbot/transfers` (and transfer assignment) checked
  the agent with `users.organization_id = <current org>`, but membership of other
  organizations lives in `user_organizations`. Members working in a clinic
  organization that is not their home one got `404 Agent not found` when
  pausing the AI for themselves.
- **Change:** `internal/handlers/helpers.go` adds a membership-aware user
  lookup. A user qualifies when active (for transfers), not deleted, and either
  has a live, valid `user_organizations` row for the org (validated with
  `access.OrganizationMembership`, so suspended reseller-derived memberships are
  rejected) or has no membership row at all and the legacy home-org match. Any
  error fails closed. Used by `CreateAgentTransfer`, `AssignAgentTransfer`
  (`agent_transfers.go`) and agent analytics (`agent_analytics.go`; team agent
  list scoped to the current org's teams, membership rows loaded in one query).
- **Unchanged on purpose:** error messages/status codes; the "away" check
  (`400 Agent is currently away`); no super-admin bypass; `findByIDAndOrg` for
  other models; `AddTeamMember` stays home-org only, because the team
  auto-assigner does not re-check membership (widening it first needs a
  membership filter in `internal/assignment` - follow-up).
- **Behaviour narrowing to note:** deactivated agents and removed members can
  no longer be chosen as transfer agents (`404 Agent not found`).
- **Frontend:** the UI branch already retries a rejected self-pause without
  `agent_id`, so it works before and after this backend change.

## 2. WhatsApp username senders: no more empty-phone contacts

- **Problem:** the regular (non-Coexistence, `is_smb = false`) Cloud API inbound
  path created contacts with `phone_number = ''` when Meta identified the sender
  only by a business-scoped user ID. With the unique index on
  `(organization_id, phone_number)`, later username-only senders were attached
  to the same contact, its `bs_uid` was overwritten, and replies could be
  addressed to a different customer.
- **Change:**
  - BSUID-only inbound messages resolve through the existing Coexistence
    resolver (`getOrCreateCoexistenceContact` and friends, unchanged), so each
    BSUID gets its own `bsuid:<hash>` placeholder contact with the existing
    `coexistence_*` metadata; a later message with the real phone upgrades the
    placeholder. A legacy `''` contact is upgraded only when it is proven to
    belong to a single sender; otherwise the message is refused (503, as such
    conflicts already were) until the row is repaired with the runbook.
  - Webhook parsing keeps `contacts[].wa_id` and `profile.username`.
  - `pkg/whatsapp.Recipient` never sends a placeholder as `to` (only the known
    `bsuid:`, `user:`, `event:`, `id:` prefixes; group IDs are untouched).
  - `contactutil.GetOrCreateContact` rejects an empty normalized phone;
    campaign recipient import and manual contact create reject it up front.
  - `updateContactBSUID` never changes the BSUID of a contact without a
    dialable phone (those are addressed by BSUID alone).
- **`/api/webhook` (canary path):** route, status codes, envelopes, the 200/503
  retry split, transaction boundaries and lock order are unchanged. One new
  explicit skip (anonymous sender with neither phone nor BSUID) answers 200,
  mirroring the existing call-webhook skip.
- **Release gate:** before deploying, run the read-only audit in
  `docs/runbooks/empty-phone-contacts-repair.md` (sections 2.1, 2.2, 2.5) for
  every organization and repair any empty-phone contact with two or more
  senders. Nothing in the runbook runs automatically.
- **Known follow-ups (outside this change):** reaction sends in `contacts.go`
  and template send by phone in `messages.go` still use `contact.PhoneNumber`
  directly (a `+`/blank phone on template send now returns 500 instead of 400);
  the call webhook still matches by phone first.

## Verification

- `GOFLAGS=-mod=readonly GOPROXY=off go build ./...` - pass.
- `go vet` on `internal/handlers`, `internal/contactutil`, `pkg/whatsapp`,
  `internal/access`, `internal/assignment` - pass; `gofmt -l` clean on all
  changed files.
- `go test ./internal/handlers/ ./internal/contactutil/ ./pkg/whatsapp/
  ./internal/worker/ -count=1` - pass. All pure unit tests ran and passed
  (recipient payloads, empty-phone guard, membership SQL and fail-closed rules,
  sender identity helpers).
- **Database-backed run (2026-10-02, owner-approved local containers):** the
  full suite (`go test -p 1 -count=1 -timeout 30m` over every package except
  `/test/`, no `-race`) against throwaway local containers from the CI-pinned
  images `postgres:17@sha256:e384…0449` and `redis:7@sha256:91d0…ab`:
  - 31 packages passed, including `internal/assignment`,
    `internal/contactutil`, `internal/worker`, `pkg/whatsapp` and every new
    database-backed test (cross-org self-pause, suspended reseller membership,
    removed members, analytics scoping; two BSUID-only senders get two
    contacts, phone-reveal upgrade, shared legacy row refusal, wa_id handling,
    campaign/manual create rejection).
  - The run found one test bug (a reused GORM destination struct), fixed in
    `3f475b23`.
  - `internal/handlers`: one failure, `TestApp_ServeMedia_RejectsSymlink`
    ("A required privilege is not held by the client" - Windows symlink
    privilege; media code untouched by this branch). Everything else passed.
  - `internal/database` (untouched by this branch) hit the 30-minute package
    timeout in its RLS suite on Docker Desktop, with no failing test before
    the cutoff. CI runs it with the same timeout on Linux.
  - Postgres 16 is not usable for these tests: `supportedPostgresMajor` only
    authorizes 14 and 17 (the general CI test job file still pins 16).
- Each change got two adversarial code reviews; every major finding was fixed
  (see commit messages).
