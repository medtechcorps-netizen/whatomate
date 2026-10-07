# Staging release contracts and lifecycle — PR9 draft

This is an offline prerequisite for Part A PR9. It does **not** add `stage` to
`ship.py`, change `ship.yml`, dispatch a workflow, or grant provider write access.
Production release files, including `spec_images.py` and `ship_common.py`, are
unchanged. The lifecycle protocol is implemented with synthetic tests, but real
plan/attestation adapters and workflow integration are still required. The
guards and callback interfaces are not an attestation verifier or deployment proof.

The dependency stack for this draft is PR237 at `31e7ea1` (staging setup kit)
plus PR238 at `0ceb26b` (repository canaries), based on main `0c2a57a`. The isolated
stack tip before PR9 changes is `5f62c5799410a2e86513a3e9762f443abfee0b47`.
Rebase the additive changes onto main after both dependencies merge; do not
merge a synthetic dependency stack to bypass their required checks.
The doctl compatibility fix `2a150c9` is also included as dependency commit
`a55902f`; it is separate from PR9's contract checkpoint `954ed38`.

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
python -I -S -B release/deployment/stage_fixture.py export-fixture --private-file "$env:USERPROFILE/rereply-staging/state.json" --output "$env:USERPROFILE/rereply-staging/canary-export.json"
python -I -S -B release/deployment/stage_fixture.py export-target --private-file "$env:USERPROFILE/rereply-staging/state.json" --output "$env:USERPROFILE/rereply-staging/target-export.json"
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
canary logins nor app/database secrets. A future deploy job must independently
verify staging image attestations and candidate attestations; a stored image
reference and source SHA are inputs to verification, not proof.

## Deliberate correction to the seven-secret plan

The original seven e2e values cannot reconstruct PR7's randomly generated two
non-super users, conversation/contact/sender IDs, Meta identities, namespace and
stub app secret. This draft defines three e2e inputs instead:

| Input | Source |
| --- | --- |
| `STAGING_ORIGIN` | Verified canonical origin from successful setup |
| `STAGING_CANARY_FIXTURE_JSON` | Exact contents of `canary-export.json` |
| `STAGING_STUB_CONTROL_KEY` | Existing private setup control key |

The original admin email/password, agent password, Klinik organization ID and
other organization ID inputs do not enter the e2e job. The two non-super logins
and exact organization identities are preserved inside the allowlisted export.
Bootstrap administrator credentials remain owner-setup-only. The future workflow
must update its explicit secret allowlist, first masking step and semantic tests
to match this correction; this draft creates no environments or secrets.

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
`CANARY_PRIVATE_FILE` pointing to it. A future workflow must supply no ambient
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
autoscaling, log sinks, domains, image tags or unknown fields. Actual provider
defaults have not yet been observed; any new shape requires a reviewed fixture
and narrow normalization change before live use.

The image transform changes only the four product digest leaves and preserves
the graph stub, every environment value and every other spec field. The separate
`bad-image` helper changes the full web/job selectors to the pinned bootstrap
image. All three failure-drill names are recognized only for `mode=stage`.
These are transform/validation helpers; no failure drill is runnable yet.

The canonical receipt contract includes the previous product image triplet,
candidate triplet, their independently verified source commits, before/after full spec fingerprints, before/candidate
deployment fingerprints, run, candidate, ingress and app hashes plus drill.
Private environment values participate in the spec fingerprint but never appear
in the receipt. Previous staging images may differ from the latest production
record; rollback must use the prior observed staging state. Receipt hashes bind
cross-job data; the future executor must verify their provenance and ensure the
exact candidate is still active before rollback.

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
This is still an integration dependency, not permission to substitute a callback
that returns an input unchanged.

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
are imported lazily; an absent adapter fails closed. Real target fingerprints,
the adapter implementation and workflow boundary are still merge prerequisites.

## Work remaining for full PR9

1. Implement and independently review real `stage.py` plan/attestation adapters,
   and review their integration with the bounded, masked command entrypoints.
   Confirm actual provider shapes against read-only
   staging observations and extend only narrow, reviewed normalization rules.
2. Independently review the implemented CAS/reconcile/health/rollback protocol
   and its adversarial tests. Complete provider-error and competing-deployment
   coverage as new adapter/API shapes become known; then prove the protocol on
   the owner-approved isolated staging resources.
3. Add explicit `PRODUCTION_MODES`/`PUT_MODES` and stage/drill input validation in
   `ship.py`; production rejects stage and every drill before I/O. Add only the
   planned staging names to production's forbidden ambient list. Keep existing
   production image functions byte-identical and run their unchanged tests.
4. Add the stage-only jobs to `ship.yml` with the same single `ship-release`
   serialization, existing production group and separate staging job group,
   no `uses` in provider-token jobs, credentialless checkout, exact environments,
   first masking before checkout/npm, step-scoped secrets, pinned Playwright,
   `npm ci --ignore-scripts`, and no uploaded private artifacts. Keep stage plan
   gates equivalent to promote. Update semantic workflow checks together.
5. Add the canonical run/receipt/candidate/origin-bound thirteen-check report,
   first-check `e2e-fail`, nonexistent-path `health-fail`, bootstrap `bad-image`
   execution and rollback proofs. Any non-`none` drill must never yield a passing
   promotion report. The current PR7 verifier is not that bound report.
6. After review/merge authorization and completed owner setup, verify production
   dry-run and current-latest rollback's no-PUT path, then staging health 6/6,
   canary 13/13 and all three rollback drills. Record exact runs for PR10.

No release should be dispatched while another Release run is queued, waiting or
running. A production rollback remains the existing production operation; an
emergency cancel decision belongs to the operator. This draft has no dispatch
or rollback command.

## Offline checks

```text
python -B -m unittest discover -s release/deployment -p 'test_stage*.py' -v
python -B -m unittest discover -s release/deployment -p 'test_*.py' -v
```

The stage tests use synthetic API shapes and private files, including actual
PR7 profile/scenario construction. They do not start a browser, perform HTTP,
run containers, access real credentials or claim a live stage deployment proof.
