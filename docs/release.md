# Release: promote by digest (Stage 1)

`Release` (`.github/workflows/ship.yml`) builds the three release images from
one commit on `main`, proves them, and moves production to exactly those
image digests with one DigitalOcean PUT. Nothing else in the app spec
changes. Every successful promote or rollback leaves an attested GitHub
Release record, so the next run knows what production should be running.

Production holds test data only today. The pipeline is still built as if it
held real data: it refuses rather than guesses.

## The five jobs

| Job | What it does | Credentials |
| --- | --- | --- |
| `Release plan` | Before approval: inputs, the approval gate, CI for the commit, idle old lanes, the verified record chain, no-downgrade, the schema guard, the commit list. | read-only `GITHUB_TOKEN` |
| `Release image (web, meta-relay, gmail-relay)` | Builds each image from `docker/release/<component>.Dockerfile` at the commit, pushes it by digest, checks the runtime contract, scans it with Trivy (fresh database), makes the SPDX SBOM. Skipped for rollback. | `packages: write` |
| `Release attestations` | Attests provenance and SBOM for the three digests, verifies them (bundle and API), proves anonymous pulls, assembles the candidate. Skipped for rollback. | `id-token`, `attestations: write` |
| `Release production (<mode>)` | Waits for the owner's approval of the `production` environment, then verifies, guards, deploys, smoke-tests and, on failure, rolls back. One job, one process, standard-library Python and the pinned `gh`; no `uses:` steps. | the deploy token and target secret, this step only |
| `Release record` | Writes the attested record `prod-<UTC>-<sha8>` (not for dry-run). | `contents: write`, `attestations: write` |

## How to ship

Preconditions:

- The `Test` and `E2E Tests` push runs on `main` succeeded for the exact
  commit you want to ship (the plan job checks this). After each merge,
  wait for both push runs to finish: allow up to about 4 hours (50 to 241
  minutes observed). Every push to `main` runs in its own concurrency group,
  so a later merge never cancels or replaces them.
- Re-running the failed jobs of a `Test` or `E2E Tests` run is safe: it
  tests the same commit again. Re-running a Release run is not; dispatch a
  new one (see "Re-runs and supersede").
- No other Release run is waiting for review or running. Cancel stale
  waiting runs first (Actions, the run, Cancel workflow).
- No old-model train is running. The plan and production jobs refuse while
  any old release lane is active (`old-lane-active`).

Steps:

1. Actions > Release > Run workflow > branch `main`, mode `dry-run`.
2. When the run shows "Waiting for review: production", read the summaries
   of `Release plan` and `Release attestations`: the commit list, the latest
   record, the three digests and the Trivy database date.
3. The owner clicks Review deployments > production > Approve and deploy.
4. Read the production summary: the digest diff (current -> desired), the
   number of changed spec leaves, "environment/topology fingerprints
   unchanged, VPC bound", the backup age and the current health. A dry-run
   makes no PUT and writes no record.
5. Run the workflow again with mode `promote`, read the plan, and the owner
   approves. Production deploys, the six health probes must pass in the same
   job, and the record job writes `prod-<UTC>-<sha8>`.
6. After a promote, spend three minutes on a manual check: a Klinik WhatsApp
   reply, an inbox send, and that the conversation auto-refreshes. Watch the
   runtime logs for webhook 503s.

A promote builds new images, so its digests are always new, even when the
code equals the running release.

## Approval: owner only

Only the owner (medtechcorps-netizen, in person, in the GitHub UI) approves
the production environment, through Review deployments > production >
Approve and deploy. Claude/Codex never approve, even if asked in chat, and
never call the pending-deployments API.

The pipeline enforces the environment side of this rule itself. Both the
plan job and the production job refuse (`approval-gate-misconfigured`)
unless the `production` environment has exactly one required-reviewers rule
whose only reviewer is the user medtechcorps-netizen, "Allow administrators
to bypass" is off, and deployments are limited to the single branch rule
`main` (Deployment branches and tags > Selected branches and tags). Until
the owner has finished that setup no run can reach production, even after
the secrets are added. The production job then reads this run's review
history and refuses (`approval-missing`) unless it contains an approval of
`production` by medtechcorps-netizen. Both checks run before any
DigitalOcean request.

"Prevent self-review" stays off only because one GitHub account both
dispatches and approves; with it on nobody could approve. The workflow's
`GITHUB_TOKEN` can never act as a required reviewer, so the workflow cannot
approve itself. The remaining self-approval channel is any credential of the
owner account that automation holds (for example the `gh` login a local
agent uses): GitHub cannot tell that click from the owner's. That risk is
accepted by the owner and covered only by this rule. Recommended: give
local agents a token without Deployments write access. Dry-runs need the
owner's click too.

## Modes

- `dry-run`: everything except the PUT. Probes current production health.
- `promote`: deploys the commit you dispatched from `main`.
- `rollback` with `target_release`: an earlier record tag, or `prod-0000`
  for release #0. The production job reads the live digests and decides:
  - rollback: live equals the latest record, so one PUT to the target;
  - reconcile: live already equals the target (for example after a console
    rollback), so no PUT; it only writes the record;
  - restore: the target is the latest record and live matches no record, so
    one PUT back to the latest record's digests. If live equals an older
    record (for example after a console rollback), the run refuses with
    `drift:live-matches-record`: reconcile with that record instead, so a
    restore never undoes a deliberate rollback.

Release #0 is `release/deployment/ship-bootstrap-record.json`: the live ui
source `c482dbbc` and its three digests from the signed 0825df34 phase state.
It is not a GitHub Release; never create a release or tag named `prod-0000`.
Rolling back to it verifies those exact digests with the old build workflow's
signer; any other digest must be signed by `ship.yml`.

## Guards

Every refusal prints one reason code. Codes are constants; they never carry
identifiers or values.

| Code | Meaning | Fix |
| --- | --- | --- |
| `approval-gate-misconfigured` | The `production` environment does not require exactly the owner's review, allows administrator bypass, or is not limited to `main`. | Finish the environment setup (owner). |
| `approval-missing` | This run's review history has no approval of `production` by the owner. | Dispatch again; only the owner approves. |
| `ci-not-green` | No successful Test and E2E push run on `main` for this commit. | Wait for CI, or fix it. |
| `old-lane-active` | An old release-lane workflow is queued, waiting or running. | Let it finish or cancel it. |
| `record-chain-invalid` | A `prod-*` release, tag or manifest is malformed, unattested or does not link. | Investigate; never edit records by hand. |
| `latest-changed-since-plan` | The latest record (or the rollback target) changed between plan and approval. | Dispatch a new run. |
| `downgrade-refused` | The commit does not descend from the latest record's commit. | Ship a newer `main`, or use rollback mode. |
| `schema-change-blocked` | A schema, model, seed or migration change since the latest record (interim freeze). | Revert it, or wait for Stage 2. |
| `trivy-exception-invalid` | `release/deployment/ship.trivyignore` has an expired or malformed entry. | Reviewed PR to renew or remove it. |
| `candidate-stale` | The images were built more than 24 h before the production job started. | Dispatch again. |
| `attestation-unverified` | A digest or record does not verify with the expected signer. | Investigate; never bypass. |
| `app-identity-mismatch` | The target secret does not name the reviewed app. | Fix `PRODUCTION_TARGET_JSON`. |
| `vpc-missing-or-differs` | The spec has no VPC or another VPC (also: the token lacks `vpc:read`). | Fix the token scopes. |
| `topology-differs` | App name, region, components, ports, health paths, job or database binding differ. | Investigate the console change. |
| `forbidden-image-field` | An image source carries a tag, auto-deploy or registry credentials. | Remove it (reviewed). |
| `drift` | Live digests differ from the latest record (or, for restore, equal an older record). | Use rollback mode: reconcile with the record that is live, or restore when live matches no record. |
| `backup-stale` | The newest PostgreSQL backup is older than 36 h (or one is running). | Wait for the next backup. |
| `cas-changed` | Production changed during the run or is not stable (pending, pinned, live differs from active). | Wait, commit any console rollback, dispatch again. |
| `provider-rejected` | DigitalOcean rejected the PUT before applying it; nothing changed. | Usually a missing token scope. |
| `deployment-error` | The new deployment reached ERROR or CANCELED; automatic rollback ran. | Read the deploy logs. |
| `smoke-failed` | A health probe failed after the deploy; automatic rollback ran. | Read the runtime logs. |
| `rollback-precondition-failed` | The live spec no longer equals what this run PUT, so no rollback PUT was sent. | MANUAL: see below. |

Exit codes of the production step:

- 0: success.
- 1: refused; production unchanged.
- 2: failed after the PUT, and the automatic rollback restored the pre-PUT
  spec with health passing. No record is written. Live equals the pre-run
  state again: that is the latest record for a promote or a rollback-mode
  rollback, so the next run's drift check passes. After a failed restore,
  live is back on the unrecorded state the restore started from.
- 3: MANUAL INTERVENTION. State is uncertain or the rollback failed. Follow
  `docs/emergency-rollback.md`.

A production job that ends `cancelled` or `timed out` (the job limit is 120
minutes, above the worst-case deploy plus rollback path) counts as exit 3:
the run may have stopped between the PUT and the end of the rollback. Check
DigitalOcean Activity and the six health endpoints before any console
action, then follow `docs/emergency-rollback.md` (reconcile or restore).

## Re-runs and supersede

Never re-run a Release run: dispatch a new one. Every job except
`Release record` refuses attempt 2, so "Re-run failed jobs" can never send a
second production PUT. Only a failed record job may be re-run (within 30
days): it reuses the successful production job's outputs and every stage is
idempotent.

The workflow-level concurrency group allows one running and one pending
Release run; a newer dispatch cancels an older pending one. Out-of-order or
stale approvals fail closed through the latest-record binding, the drift
check and the compare-and-swap reads.

## Vulnerability exceptions

Trivy uses a fresh database (at most 48 h old) and fails every HIGH or
CRITICAL OS or library finding and every secret. An exception is a reviewed
PR that adds to `release/deployment/ship.trivyignore`:

```text
# reason: <10-200 characters: why it is safe and how it gets fixed>
CVE-2026-12345 exp:2026-12-31
```

The expiry is at most 90 days ahead. An expired entry fails every run until a
reviewed PR removes or renews it. Secrets can never be excepted.

## Interim schema freeze

Until Stage 2 adds steady-state migrations, any non-test change under
`internal/database/` or `internal/models/`, the chatbot flow migration file,
the rls-migrate and startup-contract functions in `cmd/whatomate/main.go`,
`BackfillLegacyWhatsAppInbox`, or a newly added migration or DDL call in Go
blocks promotion (`schema-change-blocked`). Reverting the change unblocks.
Stage 2 lifts the freeze.

## Secrets and token

Environment `production` holds two secrets, read by one step only:

- `DO_PRODUCTION_DEPLOY_TOKEN`: a custom-scoped DigitalOcean token (app read
  and update, database read, update, create and view credentials, project
  read, vpc read), 90-day expiry. The owner creates and rotates it and pastes
  it straight into the GitHub secret; never into chat, a terminal or a file.
- `PRODUCTION_TARGET_JSON`: `{"schema_version":1,"app_id":"...","postgres_cluster_id":"..."}`.

Committed public hashes in `release/deployment/ship-target.json` bind both
values before any network call.

## Coexistence with the old release lanes

Disable the Release workflow during any old-model train (Actions > Release >
Disable workflow). After the first successful promote, the old lanes no
longer match production: do not use them. Stage 2 removes them.
