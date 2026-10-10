# Release: promote by digest (Stage 1)

`Release` (`.github/workflows/ship.yml`) builds the three release images from
one commit on `main`, proves them, and moves production to exactly those
image digests with one DigitalOcean PUT. Nothing else in the app spec
changes. Every successful promote or rollback leaves an attested GitHub
Release record, so the next run knows what production should be running.

Production holds test data only today. The pipeline is still built as if it
held real data: it refuses rather than guesses.

## Release jobs

| Job | What it does | Credentials |
| --- | --- | --- |
| `Release plan` | Before approval: inputs, the approval gate, CI for the commit, idle old lanes, the verified record chain, no-downgrade, the schema guard, the commit list. | read-only `GITHUB_TOKEN` |
| `Release image (web, meta-relay, gmail-relay)` | Builds each image from `docker/release/<component>.Dockerfile` at the commit, pushes it by digest, checks the runtime contract, scans it with Trivy (fresh database), makes the SPDX SBOM. Skipped for rollback. | `packages: write` |
| `Release attestations` | Attests provenance and SBOM for the three digests, verifies them (bundle and API), proves anonymous pulls, assembles the candidate. Skipped for rollback. | `id-token`, `attestations: write` |
| `Release staging` | Stage and normal promote: verifies the separate target and signed images, deploys once, checks health and restores the previous staging version on a deployment/health failure. | staging deploy token and target, this step only |
| `Release staging CRM checks` | Stage and normal promote: imports the existing synthetic fixture, runs all 13 checks and emits the receipt-bound report. | three fixture inputs on masking and import/test steps only |
| `Release staging rollback` | If staging deployed successfully but its CRM checks failed, verifies the receipt and restores that run's previous staging version. | staging deploy token and target, this step only |
| `Release production (<mode>)` | Waits for the owner's approval of the `production` environment, then verifies, guards, deploys, smoke-tests and, on failure, rolls back. One job, one process, standard-library Python and the pinned `gh`; no `uses:` steps. | the deploy token and target secret, this step only |
| `Release record` | Writes the attested record `prod-<UTC>-<sha8>` (not for dry-run). | `contents: write`, `attestations: write` |

## How to ship

Preconditions:

- The `Test` and `E2E Tests` push runs on `main` succeeded for the exact
  commit you want to ship (the plan job checks this). After each merge,
  wait for both push runs to finish: allow up to about 4 hours (50 to 241
  minutes observed). Every push to `main` runs in its own concurrency group,
  so a later merge never cancels or replaces them.
- Re-running the failed jobs of a `Test` or `E2E Tests` run is safe for the
  Release plan: it tests the same commit again, and the plan reads the
  run's latest attempt. (The old-lane driver publish still accepts only a
  first-attempt `Test` run.) Re-running a Release run is not; dispatch a
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
5. Run the workflow again with mode `promote`. Staging deploys that candidate,
   passes its health probes and all 13 CRM checks, then production becomes
   eligible for owner review. Read the plan and bound staging report; the owner
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

The pipeline enforces the environment side of this rule itself. For production
modes, both the plan job and the production job refuse (`approval-gate-misconfigured`)
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
- `promote`: first deploys and tests the candidate in staging, then deploys the
  same three digests to production after owner approval and evidence validation.
- `stage`: builds and verifies the same candidate, then deploys only to the
  separately configured staging team and runs its 13 synthetic CRM checks.
  The production and record jobs are ineligible.
- `promote-without-staging`: urgent owner-dispatched bypass when staging is down.
  Staging jobs must be skipped. All production plan, schema, approval, backup,
  attestation and health gates remain. See the break-glass limits below.
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

## Staging setup and drills

Finish [the staging owner setup](staging.md) and
[the staging target, fixture and receipt contract](staging-release-contract.md)
before using `stage`. The committed staging team and app hashes now identify the
independently reviewed real staging resources. Fixture completion, protected
environment configuration and live workflow proofs remain required. Null pins
continue to refuse deployment.
Do not substitute test hashes. Only synthetic staging data and credentials are
permitted. Production's `spec_images.py`, target and owner approval boundary
remain separate; the production command refuses `stage`, every non-`none` drill,
and any `STAGING_*` environment input before target/GitHub/provider I/O.

The `staging` environment supplies `STAGING_DO_TOKEN` and `STAGING_TARGET_JSON`
only to the deploy or rollback process. Those jobs use a credential-less clone,
the pinned GitHub CLI and standard-library Python; they have no `uses:` steps.
The `staging-e2e` environment supplies exactly `STAGING_ORIGIN`,
`STAGING_CANARY_FIXTURE_JSON` and `STAGING_STUB_CONTROL_KEY`. Its first step masks
those values and their nested strings before checkout or dependency installation.
The fixture is imported into a fresh private directory; the test refuses an
existing report, and the report verifier runs only after that exact test succeeds.
The verifier receives no staging secrets. No browser reports or fixtures are
uploaded as artifacts. The only public outputs are the validated receipt and
the report bound to its run, candidate and origin hashes.

Before every stage or drill dispatch, Claude/Codex check `gh run list` and confirm
there is no Release run queued, pending, requested, waiting or running. The single
top-level `ship-release` concurrency group is unchanged: starting another run
can replace an existing pending run. Deploy and rollback share `ship-staging`;
production retains `ship-production`. An urgent production rollback during a
stage run follows [emergency rollback step 4](emergency-rollback.md): cancel the
running Release workflow before proceeding. A cancelled or timed-out stage run
needs read-only reconciliation; cancellation is not proof of automatic recovery.

The `drill` input defaults to `none`. Any other value is accepted only in `stage`:

- `none`: require all six health probes and exactly 13 single-pass CRM checks.
- `health-fail`: probe `/_stage_drill_intentionally_missing.txt` (a missing static path that bypasses the SPA fallback) and exercise in-job rollback.
- `bad-image`: use the separately attested staging bootstrap image for web and
  PRE_DEPLOY, exercising migration/deployment failure and in-job rollback.
- `e2e-fail`: fail the first CRM check deliberately; the separate rollback job
  restores the preceding staging candidate. A drill never emits a passing report.

Deployment verifies the candidate's current CI, record-chain/plan binding,
schema/no-downgrade and Trivy policy/freshness again. Every product digest is
verified against its source commit; the previous staging version can be a signed
candidate that was never a production record. Rollback is bound to this run's
receipt, actual candidate deployment and unchanged spec. Ambiguous provider
writes are reconciled with reads, never retried blindly. A successful restoration
after a deployment/health failure exits 2; uncertain ownership or failed recovery
exits 3 and requires manual reconciliation. A successful cross-job rollback
does not turn the failed CRM check or original workflow into success.

PR9's owner-approved merge and protected production dry-run do not authorize
rollback. A current-latest rollback stops with `nothing-to-roll-back` before
backup/PUT when live images equal that record, but can PUT a restoration when
stable live images match no verified record. A prior dry-run cannot guarantee
the later outcome. Before dispatch, evaluate the latest record and live state
afresh and obtain separate authorization for conditional restoration, or use a
separately reviewed no-PUT mechanism. The required proof remains an actual
`nothing-to-roll-back` result. Normal staging and each drill also require owner
authorization; local synthetic tests do not substitute for their real run IDs.

## Promotion gate and break-glass (Part A PR10)

Normal promotes require plan, attest, deploy-staging and e2e-staging all to
succeed. Before production makes any DigitalOcean request, it checks the
canonical report and receipt hashes; this run and candidate; the candidate
source and three digests; the pinned staging app; matching nonproduction origin
hashes; the exact ordered 13 checks, 13 passes, and `drill=none`. Missing, stale
or mismatched evidence refuses the promote. No raw staging origin, fixture or
provider credential enters production. Dry-run and rollback never consult
staging evidence and require staging jobs to be skipped.

`promote-without-staging` is only for a staging outage that blocks an urgent fix.
The owner must dispatch it; the plan and production command reject another
GitHub actor. It still requires the owner's production approval. A red CAUTION
banner appears in plan/candidate/production summaries, and release notes state
`staging: bypassed`. It immediately follows the normal promote code path and
records kind `promote`; the attested manifest schema and record-chain rules do
not gain a bypass field or kind. An unchanged schema is mandatory, including
for this emergency mode. A bypass is not permission to skip a failing product
test or a production guard.

**PR10 requires all four real PR9 proofs to be recorded and reviewed before
owner-authorized merge.** The following operational proofs were independently
accepted on common source `8c3e022c78a025f9a51c80628cb059a921d4b8d8`, each on
attempt 1. Production and record jobs were skipped in all four runs.

| Required PR9 proof | Accepted outcome | Actual run |
| --- | --- | --- |
| Normal stage | Health 6/6, CRM 13/13 with retry 0 and successful bound-report verifier; workflow succeeded | [38008410104](https://github.com/medtechcorps-netizen/whatomate/actions/runs/38008410104) |
| `e2e-fail` | Intentional first CRM failure, 12 skipped; separate receipt-bound rollback restored the full prior staging spec and health 6/6 | [38002083135](https://github.com/medtechcorps-netizen/whatomate/actions/runs/38002083135) |
| `health-fail` | Typed intentional health failure; in-job restoration exited 2 with full prior spec and health 6/6 restored | [38004334377](https://github.com/medtechcorps-netizen/whatomate/actions/runs/38004334377) |
| `bad-image` | PRE_DEPLOY failed with the planned missing executable; in-job restoration exited 2 with full prior spec and health 6/6 restored | [38005896362](https://github.com/medtechcorps-netizen/whatomate/actions/runs/38005896362) |

The three failure-drill workflows remain failed by design; verified restoration
qualifies their proofs. Normal run `38008410104` passed all 13 checks and its
bound-report verifier, but GitHub withheld the `report_b64` job output because
the masker included the public origin checksum. PR10 excludes only the validated
top-level `origin_sha256` from recursive masking; the raw fixture, actual origin,
control key and all canary strings remain masked. The correction passed 436
deployment tests and independent review. **After PR10's owner-authorized merge
and exact merged-main CI, a fresh hosted `stage` / `none` run must verify receipt
and report output transport before the first production promote.** The historical
operational proof and local tests do not establish that transport result.

Delivered staging assets from normal run `38008410104` also passed all 12
portfolio scrolling cases without retries: wheel and Tab access to 10 synthetic
workspace rows and their controls for owner and partner administrator at
1440x900, 1280x600 and 1536x600. APIs were mocked; this proves delivered frontend
geometry, not backend authorization or real customer data behavior. Provider
state was identical before and after, and the following six health probes passed.
This result does not establish a production deployment of the UI fix.

After the owner reviews those proofs and authorizes PR10's merge, the owner
authorizes its protected production dry-run. Resolve the rollback check under
the separate scope above, then obtain approval for the first normal promote
that passed staging. This PR10 preparation does not authorize dispatch or promote.


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
blocks promotion (`schema-change-blocked`). Reverting the change unblocks
promotion. Stage 2 lifts the freeze.

The `schema-guard` job of the `Test` workflow runs the same check on every
pull request, so such a change fails the required `test` check before it can
merge instead of leaving `main` unpromotable. It compares GitHub's merge
commit with its merge base on `main`, so only the pull request's own changes
count, and each push to `main` with its first parent. Split or revert a
refused pull request. The check cannot see product code that rewrites
existing rows: such a pull request merges only when the owner says so, with a
data-change note in its description.

Two edge cases need care. A pull request that reverts a guarded change
already on `main` is refused too, because the revert edits the same guarded
paths. That can only happen if a guarded change reaches `main` despite the
check (a miss, or a rule added later). To merge the revert, the owner turns
off admin enforcement in `main`'s branch protection, merges it, turns
enforcement back on straight away and records both steps in the revert's
description. And a rebase merge is checked on its last commit only: a pull
request that adds a guarded edit in one commit and removes it in a later one
passes, but once rebase-merged its last commit edits the guarded path, so
merge such a pull request with a merge commit or squash it. Either way the
push run on `main` for that commit is red, so that commit cannot be promoted
and promotion waits for the next merge.

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
