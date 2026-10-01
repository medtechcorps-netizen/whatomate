# CRM production release control

This runbook governs the staged release of the ReReply CRM Omnichannel and Chat
fixes. It is intentionally fail-closed. Merging release controls, generating
release evidence, and changing production are three separate authorities.

## Exact final candidate

The final `ui` phase is bound to one immutable Git identity:

- commit: `6f25ea1919ee28856dee59d5fd121671214087e3`
- root tree: `2a2c14e83f4524d860a65a8735c16111178e7fd3`
- `frontend` tree: `4e027a24fcb34c2b4951d2c628dd63a3c67cc87e`
- `internal` tree: `a43572db8ee7e5a7cf2ccaf3e880a181c91ed183`

Refreshed 2026-09-16: the previous binding `20a47384` hung the full Go race job
for 40 minutes in
`internal/platformcompliance.TestBootstrapAtomicallyCreatesPurposeOrganizationAndIsExactlyIdempotent`
(ui validation run 35093373191). The refreshed identity is the same reviewed ui
release tree rebuilt on its lock-remediated base with the reviewed bounded
bootstrap dry-run lock test fix `50303c11`, so only
`internal/platformcompliance/bootstrap_test.go` differs from `20a47384`.

Refreshed 2026-09-23: four new immutable phase children carry only the
reviewed Embedded Signup generated-account-name fix and its regression tests.
Their respective parents remain the bounded snapshots `4f65abeb` (baseline),
`e403bab2` (bridge), `7cd028cb` (backend), and `96793290` (UI). The child
commits are `3cedc58f`, `0e805531`, `2cd61627`, and `1911174a` in that
order. They retain each phase's compile-time database role, unchanged frontend
tree, and the final UI's Booking, Commerce, Coexistence, and default-OFF AI
booking source. Do not substitute current control `main` for a phase source:
that checkout does not contain the complete reviewed final UI product tree.
The already-live production bootstrap keeps the historical `4f65abeb` source
identity, while its images and live phase describe the accepted c4cdac90 ui
deployment (see "Genesis re-entry at the accepted live phase"). The new
children are targets, not a rewrite of observed production history.

Refreshed 2026-10-01: twelve axios advisories published 2026-09-30 failed every
phase source's frontend audit (axios is a production dependency, so it cannot
be allow-listed). Four new immutable children of `3cedc58f`, `0e805531`,
`2cd61627`, and `1911174a` change only `frontend/package.json` and
`frontend/package-lock.json`: axios 1.18.1 to 1.20.0, dompurify 3.4.13 to
3.4.16, brace-expansion 5.0.9 to 5.0.12, and, for the baseline, bridge and
backend lockfile, js-yaml 4.3.1 to 4.3.2. The children are `0267981e`,
`0804f91e`, `78632dc4`, and `6f25ea19`, kept on the
`claude/rebaseline-<phase>-deps-20261001` branches. Their `internal` trees are
unchanged, so every phase keeps its compile-time database role and Go source.
The frontend audit allow-lists are now empty.

The release contains Klinik-only WhatsApp reply hardening and the existing
authorized-client realtime, unread-marker, and late-layout autoscroll fixes.
It does not authorize new tenants, Meta routes, callbacks, Page subscriptions,
or arbitrary environment values. Before baseline planning, the synthetic-canary
bootstrap may append only the dedicated synthetic Klinik organization to the
existing legacy WhatsApp reply allowlist. Preserve every existing allowlist
entry and keep the non-Klinik fixture excluded. Perform that one-value change by
an exact full-spec compare-and-swap, wait for the replacement deployment to be
healthy and active, then rebaseline the production contract and regenerate all
release evidence on the resulting control SHA.

### Reviewed fixed-source lineage

Each selected phase descends, one additive child at a time, from its previously
reviewed lock-fixed source: lock-fixed source, bounded snapshot, 2026-09-23
name-fix child, then the 2026-10-01 frontend dependency-refresh child. The exact parent, full changed-file/status/mode/before-and-after blob
inventory, and root/frontend/internal trees are verified independently. Preserve
both historical dependency layers: lock-only child to x/crypto-fixed source, then
go.mod/go.sum-only child to the original source. The new UI lock belongs to this
fixed snapshot, not a rewrite of either historical remediation.

The bounded fixed scope includes inbox identity/Pause controls, Meta Coexistence
source capability, staff Booking and lifecycle controls, individual lead-card
Archive and packages, and default-OFF AI booking. Existing tenant/custom-role
grants and real consenting Klinik/Meta delivery still require separate live
acceptance; merging these sources does not activate AI booking.

The UI validation harness applies only the reviewed completion patch to exact
input blob 799804fd7846167f0bb7c0d1d3e5dad3cbc3eefd, producing
eb5c656b2d4747bcfa6089cb361e4bd7bbea41e0. The common outcome-wait patch is
already incorporated and must not be applied twice. Require the complete
ChannelsView suite and all four delayed-WebCrypto selectors at 25 ms; only the
single test file may differ from the selected immutable UI source.

After the final protected control merge, regenerate all four source validations,
image sets and aggregate/capsule authority. Local compatibility results and parent
main CI do not replace exact new-source workflow evidence, production readiness,
or the separately authorized coordinated rollout.

### Compile-time database phase authority

The four PostgreSQL rollout roles are four independently compiled immutable
sources. A test that exercises all four branches from one integrated checkout
is useful coverage, but is not release evidence for four roles. Every reviewed
phase must have a distinct commit, root tree, and `internal` tree in
`release/exact-sources.json`; sharing a `frontend` tree is allowed when a phase
has no UI delta. Production code has no environment, command-line, linker, or
configuration selector for this role. Its sole authority is the package-private
`const compiledRLSMigrationPhase` literal in
`internal/database/postgres.go`.

The required database behavior is:

| Phase | Exact legacy database | Exact future database |
| --- | --- | --- |
| `baseline` | prepare/verify legacy only; never activate future | verify read-only and remain repeatable |
| `bridge` | run the complete coordinator and terminal future activation | verify read-only and remain repeatable |
| `backend` | reject before mutation; never bootstrap legacy | require and verify future read-only |
| `ui` | reject before mutation; never bootstrap legacy | require and verify future read-only |

The manifest pins `release/validation/verify_database_phase_compatibility.sh`
by SHA-256. The ordinary Test workflow invokes it only for the literal compiled
into that checkout. Exact source validation invokes the control checkout's
pinned harness against the selected clean source and PostgreSQL 17, publishes
the canonical evidence, and makes `Exact database phase compatibility` a
required gate dependency. Image publication independently reruns that harness,
requires the same named validation job in the referenced validation attempt,
and binds the canonical result to `image.json`, `scan.json`, and the signed exact
source predicate. The digest-only release-set schema and its exact 14-artifact
boundary remain unchanged; aggregation accepts image identities only when all
three carry the identical source/workflow-bound compatibility result.

Do not dispatch source validation or image publication for the new database
contract until all four complete dependency closures have been exported and the
reviewed commit/tree tuples have replaced the existing pins together in the
manifest and both release workflows. Placeholder hashes, a dirty checkout, or a
literal-only source lacking the shared coordinator and tests fail closed.

## One-time protected CRM fixture setup

The fixture controls are a one-time bootstrap, not a replacement release lane.
They create only the two reserved synthetic organizations, their dedicated
least-privilege logins and explicitly selected manual licenses, one dedicated
Meta integration/account, two signed synthetic inbound messages, and the
Klinik-only allowlist append. They neither create a Meta app nor configure a
phone webhook override. Account creation has a possible nested WABA subscription
POST; subscription recovery is disabled in this first implementation. A failed
create followed by no local account is quarantined: the nested Meta POST may
have succeeded before the product transaction rolled back.

Local tests and merging this code do not establish hosted adapter or production
authority. Use this exact order:

1. Review and merge the controls; wait for exact-main Test/E2E success.
2. Run one separately authorized `claim-test` of
   `provision-production-crm-canary-fixture.yml`. It has no environment and no
   product, Meta or provider credential. It must prove fresh artifact finalization
   succeeds and a second controller cannot obtain the same claim. Keep that
   exact successful run/artifact proof. A local fault mock is not this proof.
3. Register one canonical public request containing only `schema_version:1`,
   `control_sha`, `operation_sha256` and `descriptor_sha256`. The selector
   descriptor is protected; passwords are excluded from its public digest.
   Generate the two login local parts once using `generate_registration`:
   independent 256-bit lowercase base32 values under an explicitly controlled
   fixture email domain. Freeze these values before the intent, and never
   regenerate them after an issued operation. This provides collision resistance,
   not database-proven historical absence: the product can restore a soft-deleted
   same-email user, and live REST inventory cannot disprove that history.
4. Preconfigure `rereply-production-crm-fixture` with the sole custom branch
   policy `main` and a required-reviewer rule. Dispatch one `execute` run. Its
   intent job signs and uploads the immutable request/control binding before
   the executor waits for approval. While the executor is genuinely waiting,
   install its exact origin run/attempt=1, intent artifact ID/API digest/content
   digest, YAML job key `execute`, request and registration hashes, and approval
   expiry into the protected execution authority. Verify the actual waiting
   job if GitHub exposes its numeric ID; the runtime independently requires a
   unique matching active job and binds every burn to that numeric ID.
   Approve only this run. Do not use a sleep in an already-started job as an
   environment-secret refresh mechanism.
5. The executor verifies current protected main, its exact immutable intent,
   the prior claim-only proof and the provider predecessor before its first
   effect. It performs only the fixed ordered sequence. Each claim is consumed
   before the product/provider call. All product/provider mutation requests
   have zero retries and no redirect following. After the allowlist append,
   require stable live/active spec equality, unchanged source bindings, a new
   ACTIVE deployment and two full readiness rounds.
6. Keep the signed content-free terminal result. Rebaseline the production
   contract in the separate three-path window, merge, and regenerate all source,
   image, capsule and applicable driver evidence only after the last main move.
   No existing release evidence survives the allowlist deployment/rebaseline.

### Claim semantics and limitations

The pinned `actions/upload-artifact` JavaScript bundle is checked by commit,
Git blob and SHA-256. The Python executor runs it as a child of an exact pinned
JavaScript action host, which supplies the Actions runtime authority directly
in memory. Runtime tokens never pass through `GITHUB_ENV`, outputs, command-line
arguments or log files. All upload inputs are explicit, including
`overwrite:false` and `if-no-files-found:error`, with exactly one `claim.json`.

A successful fresh create/upload/finalize return in that same living process
creates a single in-memory send permit. Artifact readback verifies this permit;
it cannot create one. The permit is consumed before calling the fixed wrapper.
Conflict, incomplete or duplicate output, failed readback, upload ambiguity,
process loss, a second invocation, another run/job/attempt, or missing/deleted/
expired evidence grants no send permission. All contenders use the same
original-run/stage name. A later reconciliation never invokes the uploader.

Burn records reserve conservative **possible-attempt upper bounds**, not exact
observed remote execution counts: one product wrapper and at most one nested
Meta subscription for account creation. No exactly-once completion claim is
made after timeout/408/EOF/5xx. The pinned artifact client may retry claim RPCs;
this does not authorize retrying a product/provider mutation. The Actions runtime
token itself also supports artifact deletion: no deletion/overwrite relies on
pinned reviewed code and trusted GitHub administration, not on a cryptographic
create-only bearer. Artifact retention expiry never replenishes an operation.

### Temporary credential custody and read-only rehydration

One explicit bootstrap-only custody exception allows the protected fixture
executor to consume the original dedicated inputs. Its environment secret names
are exactly:

- `CRM_CANARY_FIXTURE_AUTHORITY_JSON`: immutable original execution, expiry,
  registration, hosted-adapter proof and provider target/prestate bindings.
- `CRM_CANARY_FIXTURE_INPUT_JSON`: fixed selector descriptor, original registered
  login identities, super-admin login, two distinct generated fixture passwords,
  and dedicated Meta access/app/verification credentials. No customer identity
  or shared Meta asset is an acceptable substitute.
- `DO_PRODUCTION_FIXTURE_READ_TOKEN`: dedicated GET-only app authority.
- `DO_PRODUCTION_FIXTURE_UPDATE_TOKEN`: dedicated, separately scope-reviewed
  app-update authority for only the exact full-spec allowlist append.

The executor has no environment-secret-write capability. These inputs must not
be copied to repository/organization secrets or the steady-state canary
environment. Their readers and lifetime are limited to the reviewed bootstrap
and later read-only descriptor reconstruction; revoke the bootstrap admin/Meta
transport/provider capabilities after the separately reviewed handoff/closure.
The two fixture passwords and runtime webhook secret retain their separately
reviewed runtime custody. Never publish credential-bearing request hashes as
password oracles: the public effect policy substitutes fixed custody-slot labels.

There is no raw-descriptor artifact or new persistence service. The product's
existing records retain the generated org/account/channel/contact/conversation
identities. `rehydrate` repeats complete unique readbacks using the original
protected selectors and credentials, reconstructs the raw driver descriptor in
memory, and requires its actual canonical SHA-256 to match the signed terminal
result. This hash is tested against the existing driver validator/canonicalizer;
it is not either of the earlier oracle's semantic-projection hashes. Missing or
changed rows never authorize recreation. Passwords are not obtained from GETs.

The later driver bootstrap remains a separate runtime/provider window: call
the read-only rehydration interface and transfer the descriptor directly to the
dedicated driver app. App creation, ledger binding/firewall, runtime secrets,
cost/size/count, digest readback and GitHub's five-field driver descriptor still
need their exact reviewed packet. No `/v1/execute` is run during this bootstrap.
The prior “sole shared credential” rule describes steady-state canary execution,
not this explicit temporary provisioning custody. The UI canary additionally
requires `fixture_evidence_json`, binding its runtime descriptor hash to the
successful signed setup result. Baseline/bridge/backend health probes do not
require the synthetic runtime.

### Abort and quarantine

`cleanup-production-crm-canary-fixture.yml` has no product/provider credentials
and deletes nothing. Signed intent plus complete terminal origin/attempt/job/
artifact evidence can classify `aborted_before_effect` only when the executor
never started and no effect was burned. Any started executor or burn requires
quarantine and a separately reviewed inverse. It does not improvise removal of
messages, ledger rows, users, accounts, organizations, or allowlist entries.
The `reconcile` mode is evidence-only and cannot repeat setup. Both workflows
share `rereply-production` concurrency and are included in every orphan-lock
finalizer competing-run inventory.

## Authority boundaries

The following are independent decisions and must never be treated as one:

1. Merge release-control code after protected review and CI.
2. Dispatch source validation, image attestation, rollout aggregation, and the
   read-only production-plan workflow after a fresh explicit release notice.
3. Run recovery, apply or rollback, and canary manually for exactly one phase.
   A successful canary signs the new phase state; it does not dispatch another
   phase.

Until step 3 is complete, `.github/workflows/deploy-production.yml` remains the
disabled safety gate. Do not use workstation `doctl`, the DigitalOcean console,
the historical `create-deployment --force-rebuild` path, or any equivalent
bypass.

## Coordination and main freeze

Before a control merge or release dispatch:

- record the exact PR base and head;
- confirm every task working on ReReply is paused from commit, push, merge,
  workflow-dispatch, package, environment, Meta, route, staging, and production
  mutation;
- confirm `origin/main` has not moved and no release or production workflow is
  queued or running;
- confirm DigitalOcean has no pending or in-progress deployment;
- record production health, the active deployment ID, and a sanitized exact
  app-spec fingerprint.

After a control merge, record the signed merge commit and its parents and wait
for fresh Test and E2E success. Then freeze `main`. Any later commit invalidates
all dependent validation, image, capsule, and production-plan evidence.

## Evidence sequence

Run each stage serially from the same frozen protected-main control SHA.

### 1. Exact source validation

Dispatch `Validate Exact Release Source` in this order:

1. `baseline`
2. `bridge`
3. `backend`
4. `ui`

For every phase, record the run ID and latest attempt. Require the exact
repository, workflow path, workflow title, `main` ref, control SHA, Git commit,
tree identities, and successful gate job. The `ui` phase must resolve to the
candidate tuple above. A rerun invalidates evidence that referred to an older
attempt.

### 2. Exact release images

For each phase, dispatch `Build and Attest Exact Release Images` with its exact
successful validation evidence. Record the image run ID and latest attempt, all
successful jobs, every artifact ID/API digest, the release-set artifact ID, and
the release-set SHA-256. Images are authoritative only by the three reviewed
GHCR digests; tags are labels and must never authorize deployment.

The exact image gate also performs a real anonymous manifest/blob pull of all
three digest subjects from a fresh empty Docker credential directory after all
build, secret/config, vulnerability, SBOM, smoke, and attestation checks pass.
It has no package-read token or registry login. The established release package
namespaces are public and anonymously pullable; the workflow never changes
package visibility. Public visibility is effectively irreversible. If a
namespace is missing, private, recreated, renamed, or no longer anonymously
pullable, stop and obtain a separate package-administration/bootstrap review
and explicit authorization before continuing.

The producer final gate also requires two stable, complete reads of exactly the
14 reviewed current-run artifacts. Automatic Docker build-record artifacts are
disabled at the build step. Extra, duplicate, expired, malformed, oversized,
wrong-run, wrong-branch, or wrong-control-SHA artifacts fail closed and must not
be filtered or deleted to make a run pass. Only a green producer gate with this
stable exact-14 evidence is consumable by the four-phase aggregator. No
production plan or apply is allowed until all three anonymous exact-digest pulls
and the stable exact artifact inventory pass.

### Independent CRM canary driver image

Before provisioning the synthetic CRM canary runtime, dispatch `Publish and
Attest Production CRM Canary Driver` once from protected `main`. Its sole input
is the successful attempt-1 push `Test` run ID for that exact control SHA. The
workflow re-verifies the protected ref, live `main`, the Test workflow identity,
and the successful CRM driver protocol/build/Trivy steps before publication.

The driver version is the SHA-256 of a canonical manifest containing Git mode,
blob ID, file SHA-256, and path for exactly
`docker/crm-canary-driver.Dockerfile`, `frontend/package.json`,
`frontend/package-lock.json`, and all regular blobs below
`frontend/canary-driver/`. Record this driver-version hash beside the immutable
image digest. Tags are unique diagnostic labels and never deployment authority.

The publisher builds only `linux/amd64`, disables Docker build-record artifact
upload, rechecks live `main` immediately before pushing, scans the exact digest
for embedded secrets and unsuppressed HIGH/CRITICAL OS or library
vulnerabilities, generates an SPDX SBOM, reruns the driver unit suite, and
verifies the runtime metadata contract. It creates GitHub provenance, SPDX, and
exact driver source-binding attestations, verifies all three, then anonymously
pulls the manifest and every blob with an empty credential directory. Its final
gate requires two identical complete reads of exactly four current-run
artifacts: image, scanned, attested, and verified evidence. Extra, duplicate,
expired, malformed, oversized, wrong-run, or `.dockerbuild` artifacts fail
closed.

This is a separate support-image authority. It uses its own non-cancelling
concurrency group and does not modify or participate in `Build and Attest Exact
Release Images`, its exact 14-artifact contract, or `Assemble and Attest Exact
Four-Phase Rollout`. The driver digest is not a fourth component of the product
release set. The workflow has no deployment environment, provider credential,
runtime secret, or deployment permission.

The GHCR namespace must be public before a run can pass its credential-free
gate. If first publication creates a private package, treat that run as
non-authoritative bootstrap evidence, obtain separate explicit package-
visibility authorization, verify anonymous access, and issue a fresh dispatch
rather than rerunning the failed run. Public package visibility is effectively
irreversible.

### 3. Four-phase rollout capsule

Dispatch `Assemble and Attest Exact Four-Phase Rollout` with all four phases in
the exact order above. Require current-attempt checks, artifact inventory and
hash checks, release-set and rollout attestations, and a successful final gate.
Record its run/attempt, capsule artifact ID/API digest, and rollout-plan hash.

No stage in sections 1-3 is a production deployment.

### 4. Read-only production plan

The production-plan lane must use a dedicated DigitalOcean custom-scope token
with only `app:read` and its required `regions:read`, `sizes:read`, and
`actions:read` dependencies. Independently review the token at issuance; the
repository cannot infer provider scopes from the token string. Store it only as
`DO_PRODUCTION_READ_TOKEN` in the protected `rereply-production-plan`
environment. Store only the exact stable app ID and default ingress in a
second protected environment secret named
`DO_PRODUCTION_TARGET_JSON`; the repository stores only protected hashes of
provider identity and observed state. The secret must be schema 2's stable,
strict JSON object with exactly `app_id` and `default_ingress` string fields.
The controller derives the active deployment identity and app update timestamp
from fresh provider GETs and binds their sanitized hashes into compare-and-swap
evidence; they are not operator-supplied target fields. Do not create
same-named repository or organization secrets. Do not dispatch this lane until
that environment exists, has its review policy configured, and the token has
been verified to lack app create, update, delete, restart, deploy, registry,
and database-credential authority.

If the existing protected environment secret still has the historical
four-field descriptor, rotate it to the exact two-field schema only after this
control PR is merged and under a separate, explicit environment-change
authorization. Draft publication or merge alone does not authorize that secret
mutation.

The token-bearing job must have only `contents: read` and `actions: read`. It
must not have package, GitHub deployment, attestation, or OIDC write authority
and must never use the GitHub `production` environment. A run may create a
GitHub environment record for `rereply-production-plan`; it must create no
deployment record for the `production` environment.

The plan must re-read the exact production app state, require no pending or
in-progress deployment, and prove that current and active-deployment specs are
identical. It must keep raw provider responses, encrypted environment values,
and registry credentials in memory and emit only a fixed-schema sanitized
record.

The public contract and sanitized plan retain only hashes of the app identity,
active deployment identity, provider default ingress, and provider app
timestamp. They use semantic endpoint labels (`app` and `active-deployment`),
not concrete API paths. Raw target identifiers remain step-scoped to the
protected observation job and must never appear in logs, outputs, artifacts, or
attestations. Credentials, response bodies, full specs, individual
environment-value hashes, environment counts, and environment values remain
excluded.

The app update timestamp is volatile provider metadata, not durable rollout
lineage. Bootstrap identity, genesis, and predecessor matching bind the stable
app/deployment identity, canonical spec, environment fingerprint, non-source
projection, source mode, and exact image inventory; they do not require a
historical `app_updated_at_sha256` to remain unchanged. Each observation still
parses and hashes the current timestamp. Both complete plan GET rounds must be
identical, including that hash, and the signed short-lived plan binds it as an
exact compare-and-swap witness. Apply must match the signed timestamp on its
fresh live read and again on its immediate second pre-PUT read. Timestamp-only
provider reconciliation therefore requires a fresh production plan, but does
not invalidate already completed source validation, image, or rollout-capsule
evidence while the stable lineage remains unchanged.

Provider responses must never be written, logged, placed in step outputs, or
uploaded. The token and target descriptor are step-scoped to the isolated
GET-only controller. The attestation job receives the sanitized plan plus the
independently verified digest-only rollout capsule, and no DigitalOcean target
descriptor, database, Meta, GHCR, application, or deployment credential.

The proposed transition is valid only when it changes the four approved source
bindings:

- `omnitech-web` to the target phase's exact `web` digest;
- `meta-relay` to the target phase's exact `meta-relay` digest;
- `gmail-relay` to the target phase's exact `gmail-relay` digest;
- `rereply-rls-migrate` to the same exact `web` digest.

Every other field must remain canonically and structurally equivalent after
removing only the reviewed source fields. This includes components, run
commands, environment key/type/scope/value ciphertext, databases, instance
sizes/counts, ports, health checks,
domains, ingress rules, scaling, region, and project/VPC bindings.

The plan must be short-lived and bound to the exact control SHA, rollout capsule
run/attempt/artifact/digest, active deployment identity hash, current spec hash, target
phase, three image digests, and rollback floor. It contains no full app spec or
arbitrary patch. A separate signing job may attest only the sanitized plan and
must not receive DigitalOcean credentials.

## Genesis re-entry at the accepted live phase (2026-10-01)

### Why production runs an unsigned ui phase

The 2026-09-30 train at control `c4cdac90` applied ui (apply run 36773451426,
attempt 1, receipt `de742cb5`, source `1911174a`). Its canary, run 36774347298,
failed on a fixture-reader defect (fixed by #203) before any CRM call, so that
ui phase was never signed. A phase state is valid only at its own control, so
it can never be signed later. The owner accepted the unsigned c4cdac90 ui on
2026-09-30 after a manual smoke test. That acceptance is a recorded human
decision, not machine evidence.

### The rebaselined bootstrap

`production-app-contract.json` `bootstrap_state` now describes the exact live
state recorded by that apply receipt: deployment `b7de68d8`, spec `abd19b50`,
environment `e4a9eb41` and non-source `d70b6908` (both unchanged), and the
three ui images `c392aa3d`, `de88e5b8` and `d67dcce8`. It keeps the historical
`4f65abeb` `source_sha`. Two keys are new:

- `live_phase` is `ui`;
- `live_evidence` is static data naming the receipt: kind
  `accepted-unsigned-apply-receipt`, run 36773451426 attempt 1, artifact
  11125905071 (digest `f175ba94`), predicate
  `production-phase-apply-receipt/v1`, receipt `de742cb5`, phase source
  `1911174a`, and receipt predecessor `139bf5d1` (the c4cdac90 backend state).

In digest mode the genesis hash also binds the images, the live phase and the
hash of `live_evidence`, so the new epoch's genesis is `b7892b2c`. The
verifier constants `BOOTSTRAP_LIVE_PHASE` and `BOOTSTRAP_LIVE_EVIDENCE` pin
both values, and `sanitized_provider_parity.py` pins the verifier and contract
bytes. `live_evidence` is never fetched (the receipt artifact expires
2026-10-07T20:39Z).

### Required pre-merge verification (recorded)

Before this rebaseline was proposed, the main session verified both
attestations of the receipt online with `gh attestation verify` against the
exact receipt bytes, once per predicate type
(`https://rereply.app/attestations/production-phase-apply-receipt/v1` and
`https://slsa.dev/provenance/v1`), for repository
`medtechcorps-netizen/whatomate` and signer workflow
`.github/workflows/apply-production-phase.yml`. Both verified subject
`de742cb5`. Their verified certificates name the signer workflow at
`refs/heads/main`, signer and source digest `c4cdac90`, source ref
`refs/heads/main`, a GitHub-hosted runner, trigger `workflow_dispatch`, and
run 36773451426 attempt 1. Run 36773451426 is a `workflow_dispatch` run on
`main`, attempt 1, conclusion success. This online check is mandatory before
merging any rebaseline: the committed bundles below are bound offline but are
not cryptographically re-verified by the tests.

A fresh GET-only observation on 2026-10-01 (two identical reads 60 s apart)
matched the receipt after-state: deployment `b7de68d8`, spec `abd19b50`,
environment `e4a9eb41`, non-source `d70b6908` and all three images, with no
pending, in-progress or pinned deployment. Only the provider `app_updated_at`
metadata had moved, which the bootstrap never binds. Repeat this observation
with the reviewed parity module of the PR head (`require_provider_parity`,
phase `ui`, no predecessor) before merging. If production has redeployed, do
not merge: this evidence no longer describes live.

### Committed live evidence (test-only)

`release/deployment/live-evidence/` holds the receipt, its `.sha256` sidecar and
its two Sigstore bundles. The receipt and sidecar are byte-exact copies of the
archived artifact. Each bundle is the exact bytes of one `bundle` member of
the GitHub attestations API response for that subject. The wrapper's
`repository_id`, `initiator` and its time-limited signed `bundle_url` are
not committed. `.gitattributes` marks the directory `-text`, so line endings
are never converted.

Tests bind these files offline: the sidecar and canonical bytes; every bundle's
in-toto subject equals the receipt hash; the two predicate types; the custom
predicate equals the receipt; the SLSA workflow, commit and run; and the
transparency-log entry's payload hash, signature and certificate, plus the
signer identity in that certificate. Signature, certificate-chain and
inclusion verification is the online check above. No runtime controller or
workflow reads this directory.

Evidence is permanent. Every live-evidence file stays committed, byte for
byte: a git-history test fails if one is modified or deleted. The live phase
is monotonic: `BOOTSTRAP_LIVE_PHASE`, and every digest-mode genesis entry,
must be at or above the highest phase any committed evidence records (today
ui).

### Genesis enters exactly at the live phase

Genesis has no signed predecessor, so its target is computed from the contract
and is never an input:

- a legacy-git bootstrap enters at baseline;
- a digest bootstrap enters exactly at `live_phase`, which must be in
  `GENESIS_ENTRY_PHASES` (`ui` only).

Production has run ui, so a digest bootstrap can never again re-enter at
baseline. The policy admits exactly the linear chain plus one reviewed
re-entry edge, `{genesis, ui, 4}`.

The next train is `genesis -> ui'` at source `6f25ea19` (`UI_TARGET_SOURCE_SHA`;
it descends from the live `1911174a`). It is one apply whose event chain
restarts at 1 under the new genesis hash: plan predecessor genesis (event 0),
transition ordinal 4, intent and receipt lineage `{event 1, ordinal 4, from
genesis, kind genesis}`, and phase state `{event 1, ordinal 4,
apply-receipt}`.

The downgrade guard is enforced in these lanes:

- **Plan**: genesis targets the live phase (`verify_production_plan.py:2575`)
  through the reviewed policy edge (`:2590`) with its source pinned
  (`:2600`). A predecessor state below the live phase is rejected (`:2320`),
  and an initial state must activate the live phase (`:2363`).
- **Apply**: in `_plan_images` (`apply_production_change.py:780-803`), every
  activation from any source must target at or above the live phase of the
  exact contract the plan signed, and genesis must target exactly that phase.
  It runs in the intent job before the lock (`:1137`) and in the apply job
  before `ProductionAppClient` and the PUT (`:1389`). The live phase itself
  must equal `APPLY_REVIEWED_LIVE_PHASE` (`:984`, cross-tested against the
  plan verifier).
- **Parity**: phases below the reviewed live phase are refused before any
  callback, private input or provider request
  (`sanitized_provider_parity.py:226-228`).
- **Launch helper**: the checkout is authenticated as clean current main
  before any of its code reads the live phase (`launch_production_prerequisites.py:369-371`,
  `:582`), and the predecessor callback applies the same floor (`:684`).

`verify_production_release.GENESIS_ACTIVATION_TARGETS` keeps baseline, because
that module never sees the contract and the legacy linear lineages still use
it. A test proves that the plan and apply refuse `genesis -> baseline` for
every digest-mode contract.

### Live-phase floor induction

The remaining lanes have no live-phase rule of their own: rollback, orphan
rollback, canary signing, cleanup, finalize, and both lock-release lanes.
They rely on this induction over the events at one control:

1. **Base.** At a new control no phase state exists. Plans and phase states
   carry the control SHA and the release-policy hash
   (`verify_production_plan.py:2257-2276`), so no earlier epoch's state is a
   predecessor.
2. **Plans.** The plan refuses every predecessor below the live phase, so
   the only plan at this control is `genesis -> ui`.
3. **Receipts.** Apply is the only receipt producer, and it refuses every
   activation below the live phase. Orphan reconciliation classifies an intent
   apply already signed (`reconcile_production_orphan.py:287`, `:411`, `:674`,
   through `verify_production_release.py:1811`).
4. **States.** A phase state is built only from a change receipt and inherits
   its target: `build_phase_state` copies the receipt lineage and after-state
   (`verify_production_release.py:2144`, `:2219`). The canary signer
   (`release/canary/verify_production_crm_canary.py:1374`) and terminal
   cleanup (`cleanup-production-valkey-recovery-fork.yml:394`) accept only
   such states. Finalize (`finalize_production_orphan_lock.py:859-888`) and
   the lock-release lanes (`authorize_production_main_lock_release.py:32`,
   `reconcile_production_main_lock_release.py:134`) consume the same
   receipts and states. So every state signable at this control is ui.
5. **Rollback.** A rollback target must be a phase state signed at this
   control in the same rollout plan: `rollback-production-phase.yml:114`
   and `rollback-production-orphan.yml:114`, then
   `rollback_production_change.py:257` and `:394` for the rollout plan, and
   `:372` and `:602` for the state. The only such state is ui, and ui cannot
   roll back to itself (`:388`).

So there is **no governed rollback after ui'**. ui's floor targets (backend,
bridge) are unreachable, because no state below ui can exist at the control.
`test_apply_production_change.LiveFloorInductionTests` proves steps 2-5.

### Every future release must rebaseline

ui is terminal, phase states are bound to their control, and the plan's
genesis gate counts canary successes per control. So the next release cannot
chain from the signed ui' state; every future release must rebaseline again.
That is a data-only PR using `live_evidence` kind `signed-phase-state`, whose
shape is already defined. It also records `receipt_predecessor_state_sha256`
and `canary_sha256` to keep the epoch link.

Capture the ui' apply receipt and its attestations within 7 days, and the
phase state and its attestations within 30 days. Commit both, with sidecars
and bundles; never delete the existing evidence.

Data-only rebaseline checklist:

1. Update the constants: `BOOTSTRAP_DEPLOYMENT_ID_SHA256`, `BOOTSTRAP_IMAGES`,
   `BOOTSTRAP_CANONICAL_SPEC_SHA256` (and environment or non-source if they
   moved), `BOOTSTRAP_LIVE_PHASE`, `BOOTSTRAP_LIVE_EVIDENCE`, and any target
   source pin.
2. Update the contract `bootstrap_state` to match, and recompute
   `genesis_state_sha256`.
3. Move the genesis pin in `test_verify_production_plan.py`.
4. Re-pin parity `VERIFIER_SHA256` and `CONTRACT_SHA256`, and the Codex
   launcher's `LAUNCH_HELPER_SHA256` if the helper changed.
5. Compute every pin from `git show HEAD:<path> | sha256sum` of the
   committed LF blobs, never from a Windows working tree.
6. Run the attestation verification and the fresh parity observation again.

### Failure runbook for the re-entry train

Production is untouched through F1-F7, and main is never locked through F5.
Never relaunch the launcher after an apply has been dispatched; use the
orphan and lock lanes instead. If the apply outcome is unknown (for example,
its approvals wait past the launcher's 5400 s), the launcher logs the apply
run ID and drops the plan and recovery bindings. It records the unresolved
apply, prints `do not relaunch; use the orphan lanes`, and exits non-zero.

- **F1. Validation, image or aggregate fails.** The launcher retries a
  validation or image job once, then stops. A relaunch at the same control
  reuses the evidence. Image Trivy DB freshness is about 2026-10-06T19:09Z.
- **F2. Fork prepare fails, or a later pre-apply stage fails** (plan,
  recovery, launcher crash). Use cleanup's never-started mode if no apply,
  rollback or reconcile run exists after the fork create. Use quarantine if
  only the create intent exists. Fork creation precedes planning, so a
  plan-time genesis error costs a fork plus a never-started cleanup. Owner
  fallback: delete the fork in the DigitalOcean console.
- **F3. Plan rejected** (drift, verifier defect, or a successful canary
  already at the control). Same as F2. Keep the fork at most about 24 h old
  and the production backup at most 36 h old.
- **F4. The apply authority job fails before the lock.** Use cleanup's
  pre-mutation-failure mode. Main was never locked.
- **F5. Prelock, intent or the lock job fails before the branch mutation**
  (for example the apply live-phase guard, a plan or recovery past 900 s, or a
  rejected approval). Main is unlocked. Relaunch: the launcher drops plan and
  recovery and re-plans. Owner fallback if abandoned: delete the fork in the
  console.
- **F6. The lock is held, but the proof or lock job fails after mutating.**
  Run reconcile-production-orphan (no-mutation), then
  finalize-production-orphan-lock, then
  reconcile-production-orphan-lock-release and confirm, then cleanup's
  no-mutation mode. All accept `genesis -> ui`. Owner fallback: turn branch
  protection "Lock branch" off.
- **F7. The apply job fails before or without the PUT** (900 s expiry, CAS or
  plan observation). Production is unchanged, but reconcile returns
  indeterminate and there is no governed unlock. The owner turns "Lock
  branch" off. Approve every gate within 900 s of the plan.
- **F8. The PUT was sent and the deploy fails or times out.** There is no
  governed unlock. The owner restores the previous ui spec or deployment in
  the console, then turns "Lock branch" off. A new deployment ID makes the
  bootstrap stale, and every plan then fails closed until a reviewed
  rebaseline. Production must never be re-pointed to an unattested
  deployment.
- **F9. Deployed, but receipt validation, the gate or release authorization
  fails.** Main is locked and production runs ui'. Run, in order:
  reconcile-production-orphan (committed); verify-production-crm-canary with
  `receipt_kind` `reconciliation`; the reconciliation phase state; finalize;
  orphan lock release; terminal fork cleanup. Use manual `workflow_dispatch`
  at the same control: the launcher hard-codes `receipt_kind` `apply`.
- **F10. The unlock job's runner dies.** Run
  reconcile-production-main-lock-release, then the canary with
  `receipt_kind` `apply-reconciled`, then terminal cleanup.
- **F11. The canary fails after a successful apply.** Main is unlocked and ui'
  is unsigned. Triage, then dispatch verify-production-crm-canary as a NEW
  `workflow_dispatch` (attempt 1) within about 24 h of the apply. Never move
  main in between. There is no governed rollback, because no signed backend
  or bridge state exists at the control. If abandoned, the owner deletes fork
  #2 in the console. Capture the ui' receipt and its attestations within 7
  days for an `accepted-unsigned-apply-receipt` rebaseline.
- **F12. The canary signs, but terminal cleanup fails.** Re-dispatch cleanup
  terminal after the recovery window has expired. Keep main frozen: cleanup
  re-validates recovery against current main. Owner fallback: delete the fork
  in the console.
- **F13. Credentials or tokens expire mid-train.** Stop at the current stage.
  The owner rotates them through the environment secret UI.

Deadlines: plan and recovery 900 s; intent 15 min; fork 24 h; backup 36 h or
less; canary about 24 h after the apply; signed receipt 7 days; phase state
30 days; receipt artifact 11125905071 expires 2026-10-07T20:39Z; fixture
evidence 2026-12-14T17:54Z. After a successful train, unfreeze `main`,
archive the chain-state files, and schedule the ui' rebaseline PR.

## Single-operator phase control

This rollout uses protected-main branch policies and dedicated GitHub
environments, but—by explicit operator decision—zero required reviewers and no
wait timer. That removes a second-person approval, not any technical gate. The
operator must dispatch every workflow manually from the exact frozen `main` SHA.
Repository-wide concurrency group `rereply-production` serializes validation,
image publication, rollout aggregation, planning, recovery, apply, canary, and
rollback.

The environments have distinct authority:

- `rereply-production-plan` contains only the GET-only DigitalOcean token and
  protected target descriptor;
- `rereply-production-recovery-create` contains separate GET-only database and
  `database:create` tokens plus the protected two-cluster target descriptor.
  Its exact secret-name inventory is
  `DO_PRODUCTION_DATABASE_TARGET_JSON`,
  `DO_PRODUCTION_DATABASE_READ_TOKEN`, and
  `DO_PRODUCTION_DATABASE_CREATE_TOKEN`.
  The GET token performs intent and post-request reconciliation reads; the
  create token is confined to the one-shot POST transport in the mutation job;
- `rereply-production-recovery` contains only the GET-only database inventory
  token and the same protected target descriptor. Its exact secret-name
  inventory is `DO_PRODUCTION_DATABASE_TARGET_JSON` and
  `DO_PRODUCTION_DATABASE_READ_TOKEN`;
- `rereply-production-recovery-cleanup` contains separate GET-only database and
  narrowly scoped `database:delete` tokens plus the protected target
  descriptor. Its exact secret-name inventory is
  `DO_PRODUCTION_DATABASE_TARGET_JSON`,
  `DO_PRODUCTION_DATABASE_READ_TOKEN`, and
  `DO_PRODUCTION_DATABASE_DELETE_TOKEN`. The GET token performs discovery and
  two stable absence reads;
  the delete token is confined to the single DELETE transport after exact
  signed cleanup authority is verified;
- `rereply-production-apply` contains the narrowly scoped provider mutation
  credential, stable two-field target descriptor, dedicated
  `GH_PRODUCTION_BRANCH_READ_TOKEN`, and separately scoped
  `GH_PRODUCTION_BRANCH_LOCK_TOKEN`; each credential is referenced only by the
  exact read, provider-mutation, or branch-mutation step that requires it;
- `rereply-production-orphan-reconcile` contains only the GET-only production
  observer token, stable two-field target descriptor, and dedicated branch-read
  token used to classify an incomplete locked mutation;
- `rereply-production-orphan-finalize` contains only the dedicated branch-read
  and branch-lock credentials. The branch-write credential is visible only to
  the exact one-shot lock-release step and no job in this environment receives
  a DigitalOcean or application credential;
- `rereply-production-orphan-observe` contains only
  `GH_PRODUCTION_BRANCH_READ_TOKEN` with Administration read and repository
  metadata access. Actions, run, job, artifact, and attestation reads use the
  permission-scoped job token. This environment contains no branch-write,
  DigitalOcean, provider, application, database, registry, package, Meta, or
  route credential;
- `rereply-production-canary` has the exact secret-name inventory
  `CRM_CANARY_PUBLIC_TARGETS_JSON` and `CRM_CANARY_SYNTHETIC_DRIVER_JSON`.
  The latter is consumed only for `ui` and is schema version 1 with exactly the
  reviewed driver HTTPS URL, driver-version SHA-256 label, canonical fixture-
  descriptor SHA-256, and the base64 32-byte request/result HMAC key.
  Driver fixture descriptors, fixture logins, the synthetic webhook-signing
  secret, and the execution-ledger credential do not belong in this GitHub
  environment.

No job may fall back to repository, organization, workstation, or legacy
secrets. The canary signer has no environment, provider token, application
credential, or synthetic-driver key.

Before apply or rollback is enabled, the repository must have exactly one
branch-protection rule whose pattern is exactly `main`. It must initially be
unlocked, have administrator enforcement enabled, and have fetch-and-merge
through the lock disabled (`lockBranch=false`, `isAdminEnforced=true`, and
`lockAllowsFetchAndMerge=false`). The protected apply and orphan-control
environments must contain `GH_PRODUCTION_BRANCH_READ_TOKEN` with fine-grained
Administration read and repository metadata read access. The existing orphan
mutation reconciler additionally requires Actions read because its pinned
script authenticates both branch protection and original run evidence through
that token. The new orphan lock-release observer does not: it uses the branch
token only for GraphQL branch-rule reads and the explicitly scoped job token
for Actions, artifact, and attestation reads.
`GH_PRODUCTION_BRANCH_LOCK_TOKEN` has only the Administration write plus
metadata access needed to toggle that exact rule, and appears only in the one
lock-acquire and one lock-release mutation step. This setup and token
installation are separate environment/repository changes and are not
authorized by this PR.

Before sending `lockBranch=true`, the mutation lane creates and uploads an
exclusive `0600` root marker, then creates, uploads, and attests the strict v2
mutation intent. That signed intent binds the exact upstream evidence,
before/desired state, mutation fingerprint, target-route hash, rule, root
marker, run, attempt, and control SHA while describing the lock as planned.
Lock acquisition is one GraphQL mutation followed by a GET reconciliation of
the exact rule. A separate tokenless job then creates and attests the post-lock
proof that binds the signed intent and root marker to the GET-confirmed locked
projection. The provider job cannot start without that proof. The lane keeps
`main` locked while it rechecks every authority, performs at most one provider
PUT, uploads the provisional receipt, and completes the signed receipt gate.

Only the separate success-only unlock job may send `lockBranch=false`. Before it
can start, a credential-free authorization job re-authenticates the exact eight
successful pre-unlock jobs, five existing artifacts, signed receipt
attestations, current `main`, root marker, v2 intent, lock proof, and locked
rule. It creates an exclusive JSON/hash pair, signs both provenance and the
strict release-authorization predicate, uploads that sixth artifact, and binds
an exact ten-minute validity window. The ninth, token-isolated unlock job
rechecks the nine-job/six-artifact inventory, both authorization attestations,
the original signed receipt, and the locked rule. It validates authorization
freshness again after the final locked-rule read and immediately before its sole
unlock mutation. If the process survives the send, a fresh read reconciles the
exact unlocked rule. A failure, cancellation, timeout, missing receipt, or
failed gate before that send intentionally leaves `main` locked.

A failed, cancelled, or timed-out normal unlock job can be reconciled only by
`Reconcile Production Main Lock Release`, within 30 minutes of the source job
completion and without rerunning any source job. The shared three-job lane first
authenticates the original dual-attested authorization and receipt, the exact
first eight successful source jobs, and an unlock step that started while the
authorization was valid. The authorization job-inventory hash is bound
transitively through that dual-attested authorization rather than duplicating
its full job snapshot in the reconciliation; the terminal source inventory
cross-binds the same job IDs and names. It then uses only the protected
branch-read capability to perform three identical observation rounds; every
round binds a main read,
branch-rule query, and second main read. The final signed reconciliation proves
zero mutations and the unique, administrator-enforced unlocked rule. Canary
receipt kinds `apply-reconciled` and `rollback-reconciled` require both the
original signed receipt and this exact signed reconciliation. Their schema-v2
phase state carries full immutable bindings to both artifacts; either binding,
operation, kind, attempt, artifact name, digest, or file-hash mismatch fails
closed. Direct successful apply/rollback receipts continue to emit schema-v1
phase state. Do not manually unlock, replace the rule, rerun only failed jobs,
or bypass either evidence chain.

`Reconcile Production Orphan` is GET-only. It authenticates the exact v2 intent,
locked rule, upstream evidence, optional original receipt, and a hash-only
single-operator assertion. It also signs the exact GitHub provider-job/step
projection; `no-mutation` is possible only when the capability job and all of
its steps are completed as skipped. Executed, failed, cancelled, missing, or
ambiguous provider-job evidence cannot be called `no-mutation`. Committed and
already-receipted outcomes must pass the normal canary before any orphan lock
can be released. `Rollback Production Orphan` consumes a committed
reconciliation, retains the inherited lock, creates a new signed intent and
GET-confirmed lock proof, performs one permitted compensation PUT, and emits a
receipt for the normal canary. It never acquires or releases the branch lock.

For a v2 `no-mutation` classification only, the two exact live GET rounds must
still be identical to each other, including `app_updated_at_sha256`, but that
live timestamp hash may differ from the signed pre-mutation value. App identity,
default ingress, active deployment identity, canonical spec, environment and
non-source projections, source mode, and image inventory must remain exact.
Both timestamp hashes remain inside the canonically hashed and attested receipt;
this is not a relaxed mutation compare-and-swap. The exception is valid only
with no transition, no original receipt, and exact provider-job
`never_started=true` evidence. The resulting reconciliation is canary-ineligible,
cannot create a phase state, and cannot authorize an orphan rollback or any
provider mutation.

`Finalize Production Orphan Lock` is a separate four-job break-glass lane. It
accepts only one of three terminal chains: a canary-certified committed
reconciliation, signed `no-mutation` plus exact provider-job-never-started
evidence, or a canary-certified inherited-lock orphan rollback. The typed
confirmation is exactly `FINALIZE LOCKED PRODUCTION <reconciliation_run_id>
<reconciliation_sha256>`; only its hash enters an artifact. The tokenless signer
re-authenticates every exact run, attempt, job inventory, artifact API digest,
file hash, provenance, custom predicate, lineage, current `main`, and locked
rule before issuing a signed pre-unlock authorization that is valid for ten
minutes; its artifact is retained for 30 days. The next job
validates that still-fresh authority, queries the exact locked rule, sends one
`lockBranch=false` mutation, queries the rule again, and creates a provisional
post-release receipt only when the exact rule is GET-confirmed unlocked with
administrator enforcement and fetch-and-merge settings preserved. A final
tokenless gate validates, uploads, and attests the separate confirmed-release
receipt. The successful run therefore has exactly four jobs and three
artifacts: signed preauthorization, unsigned confirmed receipt, and signed
confirmed receipt. It performs no DigitalOcean, app, route, environment,
database, package, Meta, or provider mutation.

A hosted-runner loss after the unlock request can leave a truthful signed
preauthorization, an optional unsigned receipt, and an unknown/already-unlocked
rule state. Do not rerun the finalizer or manually toggle the rule. Use the
separate `Reconcile Production Orphan Lock Release` lane only after its review,
merge, protected environment setup, and exact-main checks are complete. Its
typed phrase is `RECONCILE UNLOCKED PRODUCTION <finalizer_run_id>
<preauthorization_sha256>`; only the phrase hash enters its signed assertion.

That three-job lane is observation-only. It authenticates the exact failed,
cancelled, or timed-out four-job finalizer, the exact signed preauthorization,
and the complete one-or-two-artifact source inventory. It observes source
receipt truth three times: when signing the operator assertion, again before
the two-round branch observation, and immediately before signing the final
reconciliation. The first two receipt-truth records are embedded in the signed
assertion and reconciliation. Each contains a UTC timestamp, exact canonical
source-artifact inventory hash, unsigned receipt binding when present, exact
provenance/custom-predicate query counts and query hashes, and verification
hashes when both attestations exist. The third recheck records its timestamp,
classification, inventory hash, and exact receipt-truth file hash in the gate
summary immediately before signing. The stable classification, artifact
binding, and exact 0/0 or 1/1 attestation counts must agree across all rounds.

The only permitted source classifications are:

- `preauthorization-only`: no unsigned receipt and the deterministic empty
  attestation lookup state;
- `unsigned-unattested`: an exact controller-validated unsigned receipt with
  zero provenance and zero custom-predicate attestations;
- `attested-receipt-upload-incomplete`: the exact unsigned receipt has exactly
  one valid provenance and one valid release-receipt attestation, but the final
  signed-named artifact upload did not complete.

An exact signed release-receipt artifact with both attestations means the
original finalizer already completed and the reconciliation aborts. A partial,
duplicate, malformed, changed, or lookup-ambiguous attestation state fails
closed. The observer performs two identical reads of current `main` and the
unique unlocked, administrator-enforced rule, then signs only
`observed-unlocked-after-incomplete-finalizer`. It never claims a mutation
count, sends a branch mutation, unlocks a rule, or receives any production
provider capability. A still-locked, moved, missing, duplicate, or otherwise
unknown rule remains **NO-GO**; no generic or manual unlock is allowed.

Recovery uses DigitalOcean's provider-native Valkey fork operation. DigitalOcean
documents that a fork contains the source cluster's latest transaction, data,
and configuration at fork time and requires RDB persistence. This provider
contract replaces the unavailable per-key Managed Valkey ACL design; no Valkey
endpoint, username, password, marker, or data-plane command is exposed to
GitHub. DigitalOcean does not expose a per-key checksum, transaction watermark,
or parent-lineage field for this API, so the signed evidence claims only the
reviewed provider copy contract—not an independently measured key-by-key copy.

`Prepare Production Valkey Recovery Fork` first uses a GET-only token to bind an
immutable signed intent to the exact source identity, provider name,
configuration, topology, firewall, phase, control SHA, rollout plan, and
deterministic operation name. Only after that intent is attested may a separate
`database:create` token issue one POST. A response is authoritative only when a
strict clean `201` body exactly matches the request and two stable provider
observations. Any timeout, `408`, `429`, EOF, redirect, malformed response, `5xx`,
or post-send response mismatch is reconciled with GETs for quarantine/cleanup
only; it is never accepted as release authority and the POST is never repeated.

The read-only recovery observer binds the App Platform contract region `sgp`
to DigitalOcean's reviewed managed-database region `sgp1`. PostgreSQL, source
Valkey, and the provider-created Valkey fork must report the exact contract
versions. Source and fork must have equal sanitized topology and configuration
fingerprints. Each firewall must contain exactly one `app` trusted-source rule,
whose value hashes to the exact production app identity in the signed contract;
the create request installs that same rule atomically. Empty, public, IP/CIDR,
multi-rule, or arbitrary-app admission fails closed. The fork must be distinct
and no older than 24 hours, and two complete GET rounds must be identical. This
admits the existing production app to the temporary fork as an explicit residual
risk; GitHub receives no fork endpoint or credential and the fork remains
operation-scoped. Raw provider identifiers remain only in protected descriptors
and process memory. PostgreSQL readiness additionally requires a completed
backup no older than 36 hours with a positive finite size; an empty, stale,
in-progress, or malformed backup inventory fails closed.

The dedicated tokens use no legacy or broad API scope and never include
`database:view_credentials`. The read token has `database:read` plus
`regions:read`, `sizes:read`, and `actions:read`. DigitalOcean requires those
same read dependencies on the create and delete tokens. The create token adds
only `database:create` and `vpc:read` (required because the exact create body
copies `private_network_uuid`); the cleanup token adds only `database:delete`.
The controller still routes GETs through the separate read token.

For each phase, perform these manual actions in order:

1. Dispatch `Prepare Production Valkey Recovery Fork` and record the exact
   successful create-receipt run, attempt, artifact ID/API digest, and file hash.
2. Dispatch `Plan Production Rollout (Observation Only)` and record its exact
   short-lived signed observation plan.
3. Dispatch `Verify Production Recovery Readiness` with the exact production
   plan and fork receipt, then record its exact successful signed evidence.
4. Dispatch `Apply Production Phase` for the exact signed plan and recovery
   evidence. It performs one digest-only full-spec update after immediate
   compare-and-swap checks and emits a provisional, attested apply receipt.
5. Dispatch `Verify Production CRM Canary` with the exact apply receipt
   descriptor. Only its successful final gate creates the signed phase state.
6. After the 15-minute recovery evidence has expired, dispatch `Cleanup
   Production Valkey Recovery Fork` with one canonical exact mode. `terminal`
   requires the create receipt, expired recovery evidence, and successful phase
   state. `never-started` requires a successful create receipt, an exact typed
   discard phrase, zero later apply/rollback/reconciliation runs, and zero
   nonterminal recovery runs. It permits either no successful recovery run or
   exactly the supplied successful recovery evidence after that evidence has
   expired. `pre-mutation-failure` is a separate receipt-backed exception for
   an apply attempt that failed inside the exact read-only authority boundary:
   it requires expired recovery evidence, one attempt-one failed apply run, the
   exact nine-job inventory, the reviewed authority-step failure, all eight
   lock/intent/provider/receipt jobs skipped with no steps, and an exact empty
   run-artifact inventory. Canonical job and artifact inventory hashes enter the
   typed cleanup descriptor. The workflow also requires exactly one supplied
   recovery run and one supplied apply run since fork creation, with no later
   canary, rollback, orphan rollback, or reconciliation run; it never infers
   safety from logs. Its confirmation is `DELETE RECOVERY FORK AFTER
   PRE-MUTATION APPLY FAILURE <apply_run_id>`. `no-mutation` requires the
   expired recovery evidence plus an exact signed orphan reconciliation
   classified `no-mutation`. `quarantine` is limited to a failed prepare run's
   signed create intent and its exact typed phrase. Each mode permits at most
   one DELETE. Before deletion the cleanup controller proves the source is online,
   contract-bound, and still protected by the exact production-app rule. After
   deletion it requires two delayed complete source-health and fork-absence
   reads. The independent signing gate repeats those GET-only proofs with a
   read-only token before attesting the byte-identical cleanup receipt.
   The canonical cleanup descriptor binds both the current protected-main
   `control_sha` and the fork's `authority_control_sha`. If `main` moved after
   fork creation, the cleanup workflow requires the authority SHA to be an
   exact ancestor of current `main`, authenticates every supplied historical
   run and attestation at that authority SHA, and uses only the current cleanup
   controller with the authority SHA's exact signed production contract to
   validate and delete the deterministic old fork. The current control SHA still
   commits the current contract, while the receipt's explicit contract hash binds
   the historical contract that created the fork. The signed delete receipt
   binds both controls. This cross-main exception grants cleanup
   authority only; the older evidence cannot authorize planning, apply,
   canary, rollback, or a new release.
7. Stop. Recheck production and `main`. A later phase requires a new explicit
   plan, fork, recovery, apply, canary, and cleanup chain.

Signed create intents, create receipts, recovery evidence, phase states, and
no-mutation reconciliation receipts are retained for 30 days so cleanup does
not depend on the short-lived unsigned intermediates. Deleting a fork while
recovery evidence is still consumable is forbidden.

If an applied phase fails canary, do not improvise a provider rollback. Manually
dispatch `Rollback Production Phase` with the exact current authority, target
signed phase state, recovery proof, and permitted rollback floor. It emits a
provisional attested rollback receipt. Then manually dispatch the same canary
workflow with `receipt_kind=rollback`; only a successful rollback canary signs
the new target phase state.

This timestamp-metadata change does not make normal rollback or `Rollback
Production Orphan` tolerant of timestamp drift. Those lanes continue to require
the exact signed current provider state, including `app_updated_at_sha256`,
before any PUT. Any future rollback-specific tolerance requires a separate
control design and review.

If apply or rollback stops while `main` remains locked, do not dispatch a new
mutation, rerun only failed jobs, or unlock the rule. Dispatch `Reconcile
Production Orphan` with the exact v2 intent/run evidence and content-free typed
assertion. A committed or already-receipted classification must pass the normal
canary. If compensation is required, dispatch `Rollback Production Orphan`,
then canary its exact signed receipt. Only after one of the three terminal
chains described above may the operator dispatch `Finalize Production Orphan
Lock` with the exact evidence descriptors and typed confirmation. Missing,
pending, indeterminate, legacy-v1, moved-main, ambiguous-rule, stale-attempt,
failed-attestation, or non-canary terminal evidence is a hard stop.

The rollout order remains `baseline` to `bridge` to `backend` to `ui`.
After backend migration, baseline rollback is forbidden. DigitalOcean app
rollback restores code/spec but not migrated database data; database restore is
a separate manual incident action. No workflow automatically advances, rolls
back, restores a database, or dispatches another workflow.

## CRM production canary

Each phase must pass service health and an observation window before expansion:

- app `/health` and `/ready` return 200;
- Meta relay `/livez` and `/readyz` return 204;
- Gmail relay `/livez` and `/readyz` return 204;
- active service and migration-job images equal the approved digests;
- domains, ingress, environment fingerprints, and migration command do not
  drift;
- the signed receipt and CI preserve the reviewed Meta legacy, managed
  Messenger, Instagram, Gmail, and absent app-bound-route configuration.

Those last assertions are configuration/CI preservation, not live
account-specific availability probes. Before `ui`, the runtime canary exercises
only the six service live/ready endpoints. It does not claim that a particular
Meta or Gmail account route was observed, nor that app-bound route inertness was
live-tested; any such claim requires a separately reviewed synthetic probe.

The canary recomputes the exact six-label HTTPS route-contract hash before any
probe. It rejects redirects, proxy use, private/loopback DNS answers, oversized
responses, unexpected status codes, and raw response-body evidence. Apply and
rollback receipts are accepted only from their exact protected-main workflows,
successful first attempt, exact job inventory, exact artifact ID/API digest,
and verified provenance plus custom receipt attestation.

The final controlled CRM pilot additionally verifies:

- one Klinik-only outbound reply through the product endpoint and one
  synthetic inbound message through the signed product webhook boundary;
- inbound and outbound messages appear without reloading;
- unread counts and the main-navbar marker update and clear correctly;
- switching Omnichannel or Chat conversations scrolls to the latest message,
  including after late media/layout changes;
- unauthorized tenants are still unable to send through the Klinik-only path.

The `ui` probe is delegated only to a reviewed synthetic driver bound by exact
HTTPS origin/path, driver-version/config hashes, and the reviewed canonical
fixture-descriptor SHA-256. The request contains that fixture hash plus a fresh
nonce, idempotency key, control SHA, and exact change-receipt hash. Its HMAC
binds both the canonical body and the separate freshness timestamp without
transmitting the shared key. The response must echo those bindings, report one
execution for the nonce, be fresh, carry a valid HMAC, and contain exactly the
required boolean checks with every value true. The driver
uses only designated synthetic Klinik, non-Klinik, cross-organization, and
native-Chat fixtures. Customer content, message bodies, URLs, credentials,
screenshots, videos, browser traces, network traces, and response bodies must
never enter logs, outputs, summaries, attestations, or artifacts.

The inbound fixture is a WhatsApp-shaped payload signed with the dedicated
synthetic app secret and posted directly to the product's exact
`/api/webhook?workspace=...` boundary. It does not call Meta, traverse Meta
transport, prove account-specific Meta delivery, or require a Meta access
token. The app secret authenticates only this product-boundary fixture; it is
not a transport credential. The resulting persistence, websocket, unread, and
layout observations are product assertions, not evidence about Meta transport.

The driver is a dedicated synthetic-only service and must not share an
application service, customer browser identity, customer conversation, or
general-purpose automation account. Its runtime secret store has separate
boundaries: `CRM_CANARY_HMAC_KEY_BASE64` authenticates only verifier requests
and signed results; `CRM_CANARY_FIXTURE_DESCRIPTOR_JSON` identifies only the
schema-version-1 exact product origin `https://app.rereply.app`, reviewed
synthetic namespace, organizations, conversations, contacts, display labels,
sender identities, and the exact synthetic Meta business-account, phone,
shadow-channel-account, legacy-account ID/name fields;
`CRM_CANARY_KLINIK_LOGIN_JSON` and
`CRM_CANARY_NON_KLINIK_LOGIN_JSON` are dedicated synthetic browser logins;
`CRM_CANARY_META_APP_SECRET` signs only the product-boundary webhook fixture;
and `CRM_CANARY_LEDGER_DATABASE_URL` reaches only the dedicated idempotency
ledger. That database URL must carry a password and exactly the ordered query
`?sslmode=require&uselibpqcompat=true`, which keeps DigitalOcean's encrypted
connection while using libpq-compatible certificate handling. Every other
ordering, value, query key, or connection-setting override is rejected.
`CRM_CANARY_DRIVER_VERSION_SHA256`
is a non-secret,
operator-provisioned version label copied from the successful publisher's exact
build-input manifest hash and echoed against the verifier configuration. The
runtime does not derive it from its own source bytes or deployed image, and the
label alone is not image-attestation or digest authority. None of the driver-side
fixture, login, webhook-signing, or ledger values may be copied into repository
or organization secrets, the GitHub canary environment, logs, artifacts, or
attestations. The shared HMAC key is the sole credential present on both sides
and appears in GitHub only inside `CRM_CANARY_SYNTHETIC_DRIVER_JSON`.

Before any synthetic injection or reply, the driver requires the exact public
product origin to resolve only to public addresses, signs in the dedicated
users to their expected organizations, rejects any runtime descriptor whose
canonical hash differs from the separately protected GitHub binding, and
uniquely re-reads both live fixture conversations. The live organization,
contact, identity, channel-account,
legacy read-only configuration, reply capability, and descriptor bindings must
all match; the unread fixture must begin at exactly zero. The driver refreshes
and requires an open service window after the first signed inbound fixture and
again immediately before the outbound reply. Unread increment and clear are
bound to the exact synthetic conversation as well as the aggregate navbar
count.

Individual browser and product operations have a 45-second ceiling and the
signed-webhook response is bounded to 4 KiB. The complete driver scenario has
a hard 210-second deadline that initiates network abort and separately bounded
best-effort browser-context closure, inside the verifier's 240-second total
driver-request wall-clock deadline and 300-second signed
freshness window. A driver deadline, runner loss, or concurrent replay cannot
produce a successful signed result.

This repository contains the verifier/client contract, the dedicated driver
code, `docker/crm-canary-driver.Dockerfile`, and the independent publication
workflow described above. Protected Test CI and that publisher both fail on
unsuppressed HIGH or CRITICAL OS/library vulnerabilities. Publication still
does not deploy the driver, provision its ledger or fixtures, or install any
runtime secret. The `ui` phase is therefore **NO-GO** until a separate review
deploys the reviewed image by immutable digest, designates only synthetic
Klinik, non-Klinik, cross-organization, and native-Chat fixtures, records the
published driver-version hash beside that digest, provisions the dedicated
runtime secret store and ledger, and installs the protected GitHub canary
configuration under separate explicit authorization. Customer accounts,
conversations, or messages must never be used as fixtures.

For `baseline`, `bridge`, and `backend`, CRM synthetic sending is not run; the
canary still requires exact receipt lineage, image digests, migration success,
topology, route, environment, and all six health checks. For `ui`, every CRM
synthetic check is mandatory. `production-crm-canary.json` and the final
`production-phase-state.json` contain only sanitized hashes, semantic labels,
booleans, phase lineage, and rollback floors.

### One-time private driver installation

`bootstrap-production-crm-canary-driver.yml` is a separately approved, manual
setup lane, not a rollout or a synthetic test. Publishing or merging its code
does not authorize its execution. Use it only after reviewing an exact public
authorization packet and installing its matching protected setup descriptor in
the approval-required, main-only `rereply-production-crm-fixture` environment.
The descriptor freezes the separate driver target, one instance, size/cost
ceiling, existing dedicated ledger binding, and credential scope review. No
target, credential, or resource-size default is installation authority.

The public packet must bind the current control, the existing signed fixture
receipt, the successful immutable driver publisher, the private descriptor hash,
and the exact allowed effects. Private provider identifiers and the generated
HMAC key stay in the protected descriptor. The installer rehydrates only the
already-created synthetic fixture using `CRM_CANARY_FIXTURE_INPUT_JSON`; it
does not retrieve GitHub secret values or recreate missing fixtures.

The setup job uses separate protected `GH_DRIVER_BOOTSTRAP_READ_TOKEN`,
`DO_DRIVER_BOOTSTRAP_READ_TOKEN`, `DO_DRIVER_BOOTSTRAP_CREATE_TOKEN`, and
`GH_CANARY_ENVIRONMENT_WRITE_TOKEN` capabilities. These are not the existing
fixture-update or production-apply tokens. The read authority must cover the
exact GitHub evidence, branch/environment metadata, and provider target checks.
No token's display name proves its scopes. Review the declared scope envelope
at issuance; no provider mutation is used to test a token. Keep all capability
tokens out of the driver runtime.

Only one attempt-1 setup run is accepted on the packet's exact control SHA.
Any earlier or duplicate setup run on that control, deleted history, or rerun
is outside the one-shot operating contract. The workflow shares
`rereply-production` concurrency. It creates at most one new app, never updates
the production app, never creates a cluster/database/user, and never manually
rewrites a database firewall. The existing driver may initialize its dedicated
ledger table. Managed attachment must produce the exact reviewed trusted-source
addition, verified through provider reads; undocumented automatic behavior is
not assumed. Ledger credentials remain a provider-resolved, GENERAL bindable,
not an encrypted literal. Runtime secrets remain encrypted private values.

After stable ACTIVE deployment, immutable image/spec checks and two health
observations, the installer writes only `CRM_CANARY_SYNTHETIC_DRIVER_JSON` to
the canary environment through a nonlogging in-memory transfer. The five-field
configuration contains only schema, execution URL, driver-version hash,
fixture-descriptor hash and the shared HMAC key. The public-target secret and
environment protections must remain unchanged. Only a content-free signed
receipt and SHA sidecar may be published. Setup never calls `/v1/execute` and
does not satisfy the later UI canary.

Timeout, transport ambiguity, conflicting resource, unexpected provider
normalization, failed readiness or uncertain secret installation stops setup.
Never repeat app creation or roll back to an old spec. Keep any partial resource
quarantined for read-only reconciliation and a separately approved cleanup;
this installer has no delete or resume capability.

The operator's `launch_production_prerequisites.py` reader checks genuine public
GitHub evidence and protected secret-name metadata before any phase starts. Its
`audit-public` mode can review a local candidate but grants no launch or bootstrap
authority. Normal checks require a clean checkout at current protected main.
The sanitized provider adapter requires explicit private runtime injection of
`DO_PRODUCTION_TARGET_JSON` and `DO_PRODUCTION_READ_TOKEN`; it never falls back
to a broad ambient provider context or raw-provider files. It compares two
complete app/active-deployment observations with independently authenticated
predecessor evidence. These checks do not replace protected workflow approval
or the workflow's immediate pre-mutation checks.

## Immediate stop conditions

Invalidate the release and stop if any of these occurs:

- `main`, a workflow attempt, artifact, digest, attestation, or candidate tree
  changes;
- another task commits, merges, dispatches a release, or changes production;
- an artifact expires or protected CI/health is not green;
- production deployment ID, app spec, route, domain, environment, database,
  service topology, or migration binding drifts;
- a deployment is pending or in progress;
- a tag or mutable Git branch is offered as deployment authority;
- any target release digest cannot be pulled anonymously with a fresh empty
  registry credential directory;
- required production approval, backup evidence, or rollback evidence is
  missing;
- the provider-native Valkey fork is missing, duplicate, stale, not an exact
  topology/configuration copy, lacks the sole contract-bound production-app
  trusted-source rule, admits any other source, or differs across the two
  complete provider observation rounds;
- any step would expose raw specs/secrets or require a direct production bypass.
