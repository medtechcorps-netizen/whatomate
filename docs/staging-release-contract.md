# Staging release contracts and lifecycle (PR9)

PR9 implements the separate `stage` mode in `ship.yml`, a bounded staging
lifecycle, real plan/attestation adapters, and receipt-bound CRM reports.
Production `spec_images.py` remains byte-identical; production accepts only its
explicit modes and refuses staging credentials or drills before I/O. The full
release suite and synthetic boundary tests validate the code, not a live
staging deployment. No workflow is dispatched by importing or testing it.

The additive work depends on Part A PR7/PR8. Rebase it onto actual main after
both dependencies merge; do not merge a synthetic dependency stack to bypass
required checks. Real owner setup, target fingerprints and the post-merge
production/staging/drill proofs remain separate gates below.

## Owner setup prerequisite

Complete the reviewed PR8 setup in the existing separate **ReReply Staging**
team, provision the synthetic fixture once, and redeploy its Klinik allowlist.
Use only the API's exact opaque `account.team.uuid` value when calculating the
team fingerprint. A browser's team ID is not a substitute. The app fingerprint
is SHA-256 of the canonical saved app ID. Neither raw identifier belongs in Git.

`release/deployment/ship-target-staging.json` contains exactly four keys:
`schema_version`, `profile`, `team_uuid_sha256`, and `app_id_sha256`. Both hash
values are intentionally `null` until that setup is completed and verified.
Validation refuses this unconfigured state. No synthetic hash makes deployment
eligible. PR9 cannot be finalized or merged until the real fingerprints and the
remaining execution checks have independent review.

## Private exports

Keep the full setup state in its owner-only `rereply-staging` directory outside
every Git checkout. After successful allowlist redeployment, these offline
commands write new owner-only files beside it and print only a fixed result:

```powershell
$stagingPrivateDir = "$env:USERPROFILE\rereply-staging-state\rereply-staging"
py -3 -I -S -B release/deployment/stage_fixture.py export-fixture --private-file "$stagingPrivateDir\state.json" --output "$stagingPrivateDir\canary-export.json"
py -3 -I -S -B release/deployment/stage_fixture.py export-target --private-file "$stagingPrivateDir\state.json" --output "$stagingPrivateDir\target-export.json"
```

Target export requires the reviewed committed fingerprints. Fixture export can
run before those hashes are committed, but still requires successful setup and
applied allowlist metadata. Both commands refuse an existing output, including
the original state file. Neither command contacts a provider, provisions a
fixture, prints secrets, sets GitHub secrets, or mutates the owner state.

The fixture export selects only schema version, saved origin hash, and:

```text
canary.origin
canary.namespace
canary.stub_origin
canary.stub_app_secret
canary.klinik_organization_id
canary.fixture.descriptor
canary.fixture.klinik_login
canary.fixture.non_klinik_login
```

It excludes database owner/runtime URLs, Valkey credentials, all provider IDs,
bootstrap administrator credentials, stub access token, config paths, and
unrelated state. The stub control key stays a separate secret. Never paste the
full setup JSON into an environment or release artifact.

The target export contains the app, VPC, PostgreSQL and Valkey IDs, origin, fixed
staging database name, reviewed staging image/source bindings, canonical template
hash and only the template's dynamic non-secret values. It contains neither the
canary logins nor app/database secrets. The deploy job independently
verifies staging image attestations and candidate attestations; a stored image
reference and source SHA are inputs to verification, not proof.

## Deliberate correction to the seven-secret plan

The original seven e2e values cannot reconstruct PR7's randomly generated two
non-super users, conversation/contact/sender IDs, Meta identities, namespace and
stub app secret. The workflow uses three e2e inputs instead:

| Input | Source |
| --- | --- |
| `STAGING_ORIGIN` | Verified canonical origin from successful setup |
| `STAGING_CANARY_FIXTURE_JSON` | Exact contents of `canary-export.json` |
| `STAGING_STUB_CONTROL_KEY` | Existing private setup control key |

The original admin email/password, agent password, Klinik organization ID and
other organization ID inputs do not enter the e2e job. The two non-super logins
and exact organization identities are preserved inside the allowlisted export.
Bootstrap administrator credentials remain owner-setup-only. The workflow
secret allowlist, first masking step and semantic tests enforce this correction.
Creating environments and setting real secrets remain owner setup operations.

Import additionally requires public `STAGE_RECEIPT_B64`,
`STAGE_RECEIPT_SHA256`, `STAGE_CANDIDATE_SHA256` and `GITHUB_RUN_ID` inputs:

```text
python3 -I -S -B release/deployment/stage_fixture.py import-fixture --output "$RUNNER_TEMP/rereply-staging/canary.json"
```

The importer requires a fresh dedicated `rereply-staging` directory, writes only
after every validation passes, and refuses production/provider/app ambient
configuration. It compares the owner's saved origin hash, the receipt ingress
hash and the explicit staging origin, then rejects the production origin hash.
Receipt run, candidate and pinned app hashes must also match. JSON duplicate
keys and unexpected keys at every fixture boundary fail closed. The resulting
file is directly consumable by PR7 with `CANARY_PROFILE=staging` and
`CANARY_PRIVATE_FILE` pointing to it. The workflow supplies no ambient
`CANARY_*` overrides and no credentials on npm/install or unrelated steps.

PR7 re-registers the exact synthetic account in the stub before browser use, so
its in-memory account map may restart between deployments. A journal restart or
missing entry during an assertion is still a failure. CI imports existing
identities; it never provisions a new fixture or discovers IDs by display name.

## Pure stage guards

`stage_contract.py` checks local target identity before any provider capability
exists. Read-only inventory validation requires the exact opaque team hash, the
single staging app, the two expected Singapore database clusters in the expected
VPC, PostgreSQL 17, and exactly one app-only firewall rule per cluster. A caller
must exhaust API pagination before passing inventory to these guards. Detecting
a visible production app requires reads; the enforceable invariant is **zero
writes**, correcting the original plan's literal zero-request assertion.

The staging spec guard requires all four services, one PRE_DEPLOY migration job,
both PostgreSQL bindings, exact ingress and non-secret env values, and the exact
secret key/type/scope inventory. It compares both expected shape and JSON types,
rejects duplicate names and environment keys, and narrowly normalizes known
provider defaults. It does not silently accept new routes, source modes,
autoscaling, log sinks, domains, image tags or unknown fields. Any previously
unrecognized provider shape requires a reviewed fixture and narrow normalization
change before live use.

The image transform changes only the four product digest leaves and preserves
the graph stub, every environment value and every other spec field. The separate
`bad-image` helper changes the full web/job selectors to the pinned bootstrap
image. All three failure-drill names are recognized only for `mode=stage`.
These are transform/validation helpers. The lifecycle exercises health and
bad-image drills, while the frontend wrapper intentionally fails the first
staging check for `e2e-fail`. No live drill has been dispatched from this draft.

The canonical receipt contract includes the previous product image triplet,
candidate triplet, their independently verified source commits, before/after full spec fingerprints, before/candidate
deployment fingerprints, run, candidate, ingress and app hashes plus drill.
Private environment values participate in the spec fingerprint but never appear
in the receipt. Previous staging images may differ from the latest production
record; rollback must use the prior observed staging state. Receipt hashes bind
cross-job data; the executor verifies their provenance and ensures the exact
candidate is still active before rollback.

## Implemented lifecycle protocol

`stage.py` now has a stage-only inventory client that reuses the unchanged
production client's bounded requests and one-PUT accounting. Inventory paths
are explicitly allowlisted; pagination uses constructed URLs, bounded page and
result counts, stable totals and an exact next-link check. Redirects, unexpected
URLs and malformed or incomplete inventories fail closed. No firewall mutation
method is added.

`StageLane` requires three explicit adapters: a plan verifier, a product image
attestation verifier returning the verified source commit, and a staging support
image verifier. None has a default or permissive implementation. The product
verifier must independently resolve/verify the observed prior image set, since
the previous staging candidate may never have been promoted to production.
`stage_adapters.py` implements these checks using the pinned GitHub CLI. It
rechecks current CI, candidate freshness, the exact plan/record chain, schema
and no-downgrade, and Trivy policy/database age. An unknown previous staging
source is derived only from a cryptographically verified signing certificate,
then every product digest is verified against that exact source. An unsigned
registry label, predicate field or caller-provided hint cannot choose the source.

The protocol reads inventory and two stable app/deployment snapshots, rechecks
inventory and CAS through a fresh one-PUT client, reconciles the exact candidate,
requires its migration digest, checks all six health endpoints, and confirms
stability after health. An ambiguous PUT is reconciled with GETs only. A pending
or unknown deployment cannot cause a blind rollback. A terminal deployment may
leave the prior deployment ACTIVE while the app spec holds the failed candidate;
the rollback guard accounts for that provider behavior.

Rollback verifies two ownership observations and CAS, re-verifies the previous
image provenance, and uses another fresh client with its own one-PUT budget. It
requires a newly observed rollback deployment and health proof. Cross-job
rollback additionally checks receipt hash, run, candidate, app, origin, exact
candidate deployment/spec, prior spec reconstruction and both source/image
bindings. Deploy and rollback scrub retained client credentials on every exit.

Synthetic tests cover normal deployment, stale CAS, inventory drift, candidate
and prior attestation failures, ambiguous acceptance, definitive rejection,
pending timeout, migration failure, private spec preservation, health failure,
bad-image and health drills, changed rollback ownership, failed rollback and
receipt-bound e2e rollback. No live deployment has been performed by this draft.
The `stage.py deploy` and `stage.py rollback` entrypoints pop protected provider
inputs before constructing a Context or starting a subprocess. They refuse
production modes, mixed canary secrets, an unconfigured target and a foreign
workflow/run context before any provider request. Private target identities are
masked before use, and only public receipt data is emitted. Required adapters
are imported lazily; an absent adapter fails closed. The real adapters and
workflow boundary are implemented and independently reviewed. Real target
fingerprints and observed provider shapes remain merge prerequisites.

## Bound canary report and e2e drill

The canary configuration accepts only `none` or `e2e-fail`; the latter requires
the staging profile and the normal thirteen-check run. It rejects incompatible
drills before loading a profile or launching a browser. The serial test wrapper
fails the first check with a fixed message for the drill; the original thirteen
scenario assertions and helpers remain unchanged.

After a successful canary step, the workflow runs:

```text
python3 -I -S -B release/deployment/stage_report.py --private-file "$CANARY_PRIVATE_FILE" --report "$CANARY_REPORT_FILE"
```

The report step receives public `STAGE_RECEIPT_B64`, `STAGE_RECEIPT_SHA256`,
`STAGE_CANDIDATE_SHA256`, `GITHUB_RUN_ID` and `CANARY_DRILL` values, with no
`STAGING_*` credentials. It validates the exact private reporter format, thirteen
unique named passes, one attempt, no retries, no skipped checks and no extra data.
It binds the imported fixture to the canonical receipt, run, candidate, app and
origin, then emits only `{receipt_sha256,run_id,candidate_sha256,origin_sha256,
checks,passed:13}` in the original ordered check inventory. GitHub step outputs
are `report_b64`, `report_sha256`, and `passed=true`. Any deliberate drill is
ineligible even if every assertion unexpectedly passed.

The raw PR7 report has no run identity. The workflow must create fresh private
state/report paths, refuse an existing report before test execution, and verify
only after the successful exact canary step using that same imported fixture and
receipt. The report module alone does not prove test freshness. Its output is
public; its two input files remain owner-only and are never uploaded.

## Live prerequisites and acceptance

1. Complete owner setup on the isolated staging resources and compare actual
   read-only provider observations with the exact contract. Extend only narrow,
   independently reviewed normalization rules when real defaults differ.
2. Commit only the verified team/app fingerprints and configure the protected
   staging environments with the minimal exports and scoped credentials.
   The two null pins intentionally keep deployment unavailable until then.
3. After required CI, independent review and owner merge authorization, verify
   production dry-run and current-latest rollback's no-PUT path. Then authorize
   a normal stage run with health 6/6 and CRM 13/13, plus all three rollback drills.
   Record exact successful proofs for PR10; synthetic tests are not substitutes.

The original PR7 verifier remains an unbound local verifier; promotion evidence
must use `stage_report.py` after the workflow's fresh-file/test-success boundary.
No release should be dispatched while another Release run is queued, pending,
requested, waiting or running. The single `ship-release` group and production's
`ship-production` group remain unchanged. Stage deploy/rollback use `ship-staging`.
A production rollback remains the existing production operation; an emergency
cancel decision belongs to the operator. Whole-run cancellation prevents the
cross-job rollback from starting and requires read-only reconciliation; it does
not prove that staging was restored. See [the release runbook](release.md).

## Offline checks

```text
python -B -m unittest discover -s release/deployment -p 'test_stage*.py' -v
python -B -m unittest discover -s release/deployment -p 'test_*.py' -v
```

The stage tests use synthetic API shapes and private files, including actual
PR7 profile/scenario construction. They do not start a browser, perform HTTP,
run containers, access real credentials or claim a live stage deployment proof.
