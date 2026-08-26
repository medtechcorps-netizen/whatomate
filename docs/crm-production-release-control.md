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
3. Merge a separately reviewed apply/canary control and approve each production
   phase through a protected environment.

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
environment. Store the exact app ID, active deployment ID, app timestamp, and
default ingress only in a second protected environment secret named
`DO_PRODUCTION_TARGET_JSON`; the repository stores only their SHA-256 hashes.
The secret must be a strict JSON object with exactly `app_id`,
`active_deployment_id`, `app_updated_at`, and `default_ingress` string fields.
Do not create same-named repository or organization secrets. Do not dispatch
this lane until that environment exists, has its review policy configured, and
the token has been verified to lack app create, update, delete, restart, deploy,
registry, and database-credential authority.

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

## Apply and canary prerequisite

Do not apply a plan until a separate PR installs and validates all of the
following:

- required reviewers on a non-bypassable production environment;
- repository-wide `rereply-production` serialization;
- immediate pre-mutation main, evidence-attempt, artifact, active-deployment,
  and full-spec compare-and-swap checks;
- one digest-only app-spec update reconstructed independently from the signed
  plan, with no `--force-rebuild`, `create-deployment`, mutable Git source, or
  `update_all_source_versions` behavior;
- exact post-update deployment/spec/digest verification;
- phase-aware rollback floors and an explicit database backup/PITR checkpoint;
- health, route compatibility, migration, and CRM functional canaries.

The rollout order remains `baseline` to `bridge` to `backend` to `ui`.
After backend migration, baseline rollback is forbidden. DigitalOcean app
rollback restores code/spec but not migrated database data; database restore is
a separate manual incident action.

## CRM production canary

Each phase must pass service health and an observation window before expansion:

- app `/health` and `/ready` return 200;
- Meta relay `/livez` and `/readyz` return 204;
- Gmail relay `/livez` and `/readyz` return 204;
- active service and migration-job images equal the approved digests;
- domains, ingress, environment fingerprints, and migration command do not
  drift;
- existing Meta legacy, managed Messenger, Instagram, and Gmail account-health
  paths remain available;
- the new app-bound Messenger path remains inert while its descriptor is absent.

The final controlled CRM pilot additionally verifies:

- one Klinik-only WhatsApp reply and one inbound response;
- inbound and outbound messages appear without reloading;
- unread counts and the main-navbar marker update and clear correctly;
- switching Omnichannel or Chat conversations scrolls to the latest message,
  including after late media/layout changes;
- unauthorized tenants are still unable to send through the Klinik-only path.

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
- required production approval, backup evidence, or rollback evidence is
  missing;
- any step would expose raw specs/secrets or require a direct production bypass.
