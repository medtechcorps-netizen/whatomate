# CRM production release control

This runbook governs the staged release of the ReReply CRM Omnichannel and Chat
fixes. It is intentionally fail-closed. Merging release controls, generating
release evidence, and changing production are three separate authorities.

## Exact final candidate

The final `ui` phase is bound to one immutable Git identity:

- commit: `ff0c9c6b8d94a085af164e564028d25d38b0a02c`
- root tree: `6b3d030924913a562fee2b75fce318b01b421792`
- `frontend` tree: `f1cdd8186e2b724dc4bba41081578b2b003a6910`
- `internal` tree: `a1da97143c17f3d02e47250269d00f238ac0e38c`

The release contains Klinik-only WhatsApp reply hardening and the existing
authorized-client realtime, unread-marker, and late-layout autoscroll fixes.
It does not authorize new tenants, Meta routes, callbacks, Page subscriptions,
or environment values.

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
- `rereply-production-recovery` contains only the GET-only database recovery
  inventory token, the three protected database identities, two provider-bound
  Valkey TLS read descriptors, and the sentinel HMAC verification key;
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
- `rereply-production-canary` contains only the six public health targets and,
  for `ui`, the controlled synthetic-driver configuration.

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

Recovery is NO-GO until a separate, explicitly authorized environment setup is
complete. `rereply-production-recovery` must contain
`DO_PRODUCTION_VALKEY_SENTINEL_SOURCE_JSON` and
`DO_PRODUCTION_VALKEY_SENTINEL_RECOVERY_JSON`, each with exact
`host`, `port`, `server_name`, `username`, and `password` fields, plus a
base64-encoded 32-byte `DO_PRODUCTION_VALKEY_SENTINEL_HMAC_KEY_B64`. The two
Valkey users must be independently verified as read-only and restricted to
`GET` on the fixed `rereply:recovery:sentinel:v1` key. The endpoints must match
the connection authorities returned for the exact contract-bound source and
recovery cluster IDs.

The read-only recovery observer binds the App Platform contract region `sgp`
to DigitalOcean's reviewed managed-database/VPC region `sgp1`. PostgreSQL,
source Valkey, and the Valkey recovery fork must all report that exact provider
region and the exact contract versions. Source and recovery Valkey must also
have equal sanitized topology fingerprints covering size slug, node count,
private-network identity hash, and storage capacity; raw network identifiers
never enter evidence. PostgreSQL readiness additionally requires a completed,
recent backup record with a positive finite `size_gigabytes`. A missing size,
empty inventory, stale record, or any present `backup_progress` is a hard stop.

Before creating the recovery fork, a controlled process writes a fresh
content-free nonce/timestamp marker with a valid HMAC to the fixed key on the
production Valkey source. The recovery workflow reads that key live from both
clusters over TLS 1.2 or newer and requires byte equality, a valid HMAC, a
source identity/name/version bound to the production contract, an equal
contract-bound version on the recovery fork, and an issue time before the fork
creation time and within 24 hours. A merely new, online, same-shape cluster is
not recovery evidence. The marker, endpoint, credential, HMAC key, and raw
cluster identifiers never enter public artifacts. This PR does not
create users, write the marker, fork a cluster, or install/rotate any secret;
those remain separate production changes.

For each phase, perform these manual actions in order:

1. Dispatch `Verify Production Recovery Readiness` and record the exact
   successful run, attempt, artifact ID/API digest, and recovery hash.
2. Dispatch `Apply Production Phase` for the exact signed plan and recovery
   evidence. It performs one digest-only full-spec update after immediate
   compare-and-swap checks and emits a provisional, attested apply receipt.
3. Dispatch `Verify Production CRM Canary` with the exact apply receipt
   descriptor. Only its successful final gate creates the signed phase state.
4. Stop. Recheck production and `main`. A later phase requires a new explicit
   dispatch and new phase-specific evidence.

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

- one Klinik-only WhatsApp reply and one inbound response;
- inbound and outbound messages appear without reloading;
- unread counts and the main-navbar marker update and clear correctly;
- switching Omnichannel or Chat conversations scrolls to the latest message,
  including after late media/layout changes;
- unauthorized tenants are still unable to send through the Klinik-only path.

The `ui` probe is delegated only to a reviewed synthetic driver bound by exact
HTTPS origin/path and driver-version/config hashes. The request contains a
fresh nonce, idempotency key, control SHA, and exact change-receipt hash; it is
HMAC-signed without transmitting the shared key. The response must echo those
bindings, report one execution for the nonce, be fresh, carry a valid HMAC, and
contain exactly the required boolean checks with every value true. The driver
uses only designated synthetic Klinik, non-Klinik, cross-organization, and
native-Chat fixtures. Customer content, message bodies, URLs, credentials,
screenshots, videos, browser traces, network traces, and response bodies must
never enter logs, outputs, summaries, attestations, or artifacts.

This repository currently contains the verifier/client contract only. It does
not contain or provision the synthetic driver endpoint or its fixtures. The
`ui` phase is therefore **NO-GO** until a separate review implements and deploys
the controlled driver, designates synthetic Klinik, non-Klinik,
cross-organization, and native-Chat accounts, records the exact driver version
hash, and installs its protected canary configuration under separate explicit
authorization. Customer accounts, conversations, or messages must never be
used as fixtures.

For `baseline`, `bridge`, and `backend`, CRM synthetic sending is not run; the
canary still requires exact receipt lineage, image digests, migration success,
topology, route, environment, and all six health checks. For `ui`, every CRM
synthetic check is mandatory. `production-crm-canary.json` and the final
`production-phase-state.json` contain only sanitized hashes, semantic labels,
booleans, phase lineage, and rollback floors.

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
- the live Valkey sentinel is missing, unequal, stale, post-fork, has an invalid
  HMAC, or is read through an endpoint not bound to the exact provider cluster;
- any step would expose raw specs/secrets or require a direct production bypass.
