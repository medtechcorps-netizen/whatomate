# Future Gate-C Disposable Resource Ceiling

This is a fill-before-authorization template. It is not authority to create anything. Every placeholder must be replaced by an independently reviewed synthetic nonproduction value before a separate Gate-C write notice.

`CLAIM_SCOPE=gate-c-nonproduction-only`

## Fixed manifest

```text
PROTO_ID=<unique-synthetic-id>
ISOLATED_TEAM_SHA256=<hash-only>
ISOLATED_PROJECT_SHA256=<hash-only>
REGION_LABEL=<synthetic-region-label>
START_UTC=<synthetic-rfc3339>
EXPIRY_UTC=<no-more-than-12-hours-after-start>
MAX_TOTAL_COST_USD=75.00
CONTROL_SHA256=<hash-only>
RUNTIME_SOURCE_SHA256=<hash-only>
WORKFLOW_DEFINITION_SHA256=<hash-only>
CONFIG_SHA256=<hash-only>
IMAGE_SHA256=<hash-only>
APP_SPEC_SHA256=<hash-only>
VALKEY_SCRIPT_SHA256=<hash-only>
VALKEY_COMMAND_SET_SHA256=<hash-only>
FIXED_KEY_POLICY_SHA256=<hash-only>
FORK_COPY_PROOF_POLICY_SHA256=<hash-only>
CLEANUP_DESCRIPTOR_SHA256=<hash-only>
```

Raw team, project, app, database, network, deployment, and endpoint identifiers belong in a protected descriptor that is never committed, logged, or uploaded.

The four Valkey/fork policy hashes are distinct and bind the reviewed script
bytes, exact allowed command set, compiled one-key selector policy, and the
post-commit fork-copy verification procedure. Gate C must resolve them to the
exact tested bytes before any write; a generic Lua-availability result is not
evidence that a fork contains the committed marker.

## Hard resource ceiling

```text
MAX_PROJECTS=1
MAX_VPCS=1
MAX_PRIVATE_REGISTRIES=1
MAX_PRIVATE_OCI_REPOSITORIES=5
MAX_SOURCE_VALKEY_CLUSTERS=1
MAX_RECOVERY_VALKEY_FORKS=1
MAX_AUTHORITY_APPS=2
MAX_BROKER_APPS=2
MAX_LEDGER_CLUSTERS=2
MAX_SIGNING_ROOTS=2
MAX_CUSTOM_DOMAINS=0
MAX_DNS_CHANGES=0
MAX_PUBLIC_DATABASE_RULES=0
MAX_DROPLETS=0
MAX_KUBERNETES_CLUSTERS=0
MAX_PEERED_NETWORKS=0
MAX_PROVIDER_TOKENS=12
MAX_PROVIDER_ACTIONS=64
MAX_APP_DEPLOYMENTS=12
MAX_APP_REDEPLOYMENTS=4
MAX_REPLICAS_PER_APP=2
MAX_SCALE_EVENTS=4
MAX_PROTECTED_ENVIRONMENTS=12
MAX_CONTROLLER_WORKFLOWS=12
```

The two authority apps are `writer-authority` and `observer-authority`. The two brokers are `writer-broker` and `observer-broker`. Each authority/broker pair has its own ledger and signing root: exactly two ledgers and two signing roots. No ledger role, root, OIDC audience, challenge namespace, app identity, or database credential is shared. Every raw created identifier is recorded once in the protected cleanup descriptor; cleanup may act only on those recorded identities after complete-inventory equality and may never use a near-name match.

The protected cleanup descriptor is a closed object with exactly these keys and
no others: `schema_version`, `claim_scope`, `proto_id`, `team_id`, `project_id`,
`vpc_id`, `registry_id`, `repository_ids`, `source_cluster_id`,
`recovery_cluster_id`, `authority_app_ids`, `broker_app_ids`,
`ledger_cluster_ids`, `trusted_source_rule_ids`, `deployment_ids`, `action_ids`,
`token_fingerprints`, `environment_names`, `branch_name`,
`signing_root_fingerprints`, `created_at`, and `expires_at`. Raw values never
enter public evidence; the public lifecycle receipt binds only the descriptor's
canonical SHA-256.

The isolated project and VPC must contain no production peering, trusted
source, descriptor, credential, hostname, route, domain, or data. The complete
team/account inventory is checked before the first write. A same-team token
with class-wide reach to production is prohibited.

If the substrate cannot provide a private authenticated authority-to-broker path while retaining this separation, Gate C fails. A public generic broker, shared egress identity, VPC CIDR admission, or caller-selected forwarding is not an acceptable substitute.

## Allowed future package publication

At most five private, prototype-labelled OCI repositories may be requested in
a later exact notice: writer authority, writer broker, observer authority,
observer broker, and a non-data-plane lifecycle test harness. Separate images
make role reuse fail closed. Every version must be bound to the reviewed
synthetic commit and unique run attempt, scanned without HIGH/CRITICAL
suppression, and deployed by digest. Tags are not authority.

Package publication, visibility, and deletion are external writes and require explicit Gate-C authorization. Existing release packages are out of scope.

## Existing-database binding experiment

Gate C may test provider-side binding of fresh disposable databases using the
provider's existing-cluster form (`production: true` plus a protected
`cluster_name`) and provider-resolved `${db.*}` bindable variables. Those are
prototype hypotheses, not accepted support claims. The hypothesis passes only
if:

- the provider injects source, recovery, and ledger credentials at runtime;
- GitHub never receives broad data-plane credentials or signing private keys;
- read APIs do not reveal resolved secrets;
- writer is bound only to source and its ledger;
- observer is bound only to recovery and its ledger;
- removal followed by a complete redeploy eliminates the credential projection.

Failure of any condition ends the substrate experiment.

Because the managed-Valkey credential remains broader than the compiled key,
an explicit user risk acceptance is a hard prerequisite before Gate C creates
or binds either disposable Valkey cluster. Without that separate acceptance,
the experiment is NO-GO.

## Signing-root experiment

Each domain generates its root internally and persists private material only in its distinct provider-side ledger. GitHub receives only a public key and proof-of-possession. The same root must survive concurrent startup, two full redeployments, and a bounded scale test; generation rotation must revoke the prior key.

If private material must transit GitHub, roots disagree across instances, or revocation cannot be proven, Gate C fails and cleanup begins.

## Ledger and key-confinement ceiling

Gate C must provision two role-separated transactional durable ledger/signing
backends. Each must independently prove compare-and-swap fencing, monotonic
generation consumption, an append-only hash-chained audit log, encryption at
rest and in transit, point-in-time backup/restore, crash/restart recovery, and
terminal cryptographic erasure of sealed marker reconciliation material while
retaining public receipt digests. The writer and observer root namespaces,
database users, wrapping keys, backup sets, and restore credentials may not be
shared.

The authority signer revalidates the raw JWT and canonical request itself; it
does not trust gateway-parsed claims. Brokers accept only a role/phase/
generation-scoped signed capability and exact mTLS peer. If private signing
material can be exported to GitHub, stored only on ephemeral disk, or accessed
by a data-plane broker, App Platform is NO-GO.

## Exact future token-scope ceiling

Every token is fresh, expires no later than `EXPIRY_UTC`, is installed in one
exact protected nonproduction boundary, and is revoked during cleanup. Create,
update, and delete authority are never combined in one token.

```text
AUDIT = app:read,database:read,vpc:read,registry:read,regions:read,sizes:read,actions:read
APP_CREATE = app:create,app:read,regions:read,sizes:read,actions:read
APP_UPDATE = app:update,app:read,regions:read,sizes:read,actions:read
APP_DELETE = app:delete,app:read,actions:read
DATABASE_CREATE = database:create,database:read,regions:read,sizes:read,actions:read
DATABASE_UPDATE = database:update,database:read,app:read,actions:read
DATABASE_DELETE = database:delete,database:read,actions:read
VPC_CREATE = vpc:create,vpc:read,regions:read,actions:read
VPC_DELETE = vpc:delete,vpc:read,actions:read
REGISTRY_CREATE = registry:create,registry:read,actions:read
REGISTRY_UPDATE = registry:update,registry:read,actions:read
REGISTRY_DELETE = registry:delete,registry:read,actions:read
```

`api:read`, `api:write`, `database:view_credentials`, account, billing, DNS,
Droplet, Kubernetes, production-team, or combined lifecycle scopes are
prohibited. If an exact documented call needs another granular scope, Gate C
stops for review rather than widening a token.

Each mutation family has a distinct protected controller and protected
environment. Writer admission/teardown, fork create/reconcile, observer
admission/teardown, and unconditional cleanup may not share write credentials,
environment secrets, or OIDC subjects. All controllers use the same
non-cancelling prototype concurrency group, exact-main guards, attempt-one
authority, signed before/after inventories, and fail-closed GET-only
reconciliation. Every provider client, SDK, transport, and middleware retry
budget is exactly zero for mutation methods. Cleanup cannot grant
normal-operation authority.

## Network admission ceiling

- Writer broker: source only, one exact stable app or per-app private-egress identity.
- Observer broker: recovery only, one exact stable app or per-app private-egress identity.
- Authority apps: no data-store credential and no trusted-source rule.
- Runner: no direct data-plane path.
- No VPC CIDR, public wildcard, dynamic runner range, tag, Droplet, or Kubernetes admission.

The experiment must prove writer and observer identities are unique, independently allowlistable, and stable across redeployment. Shared or unstable identity is a hard NO-GO.

## Future provider-operation categories

A later exact notice may enumerate only:

1. create the isolated network, two ledgers, fresh source, four domain apps, and private registry;
2. publish exact prototype image digests;
3. attach existing disposable databases through provider-side binding;
4. add and remove exact single-app admission rules;
5. redeploy and scale solely for identity/key-persistence tests;
6. write one internally generated fixed-key marker;
7. revoke writer phase authority and delete only the ephemeral writer broker
   before one fork POST; retain the persistent credential-free writer authority;
8. reconcile that POST without retrying it;
9. admit observer to recovery only and read twice;
10. delete every recorded disposable resource and revoke every temporary authority.

No real provider client is present in this directory. These categories are a ceiling, not executable instructions.

## Required preflight and cleanup proof

The future team/account must be isolated from production and its complete inventory must match the exact declared starting set. Each created resource is captured by protected raw identifier and public identifier hash. Later writes use the captured raw identifier only after an exact GET/CAS check.

Cleanup authority becomes unconditional after the first write. During the
pre-fork teardown it deletes only the ephemeral writer broker and retains the
persistent credential-free writer authority. Terminal experiment cleanup then
deletes the observer broker, both authority apps, any quarantined broker
remnant, recovery, source, both ledgers, all prototype images, registry, and
network; it revokes roots, tokens, environments, policies, and branch. Verify
zero matching resources and zero nonterminal actions twice. A failed deletion
produces quarantine, not substitution or recreation.

## Gate-B publication prerequisite

Existing repository Test/E2E cannot be cited as complete prototype coverage.
Before any Gate-B publication request, one protected exact-head attempt must:

1. run all root-module Go tests, including race tests where the protected runner
   supports them;
2. run `python -B -m unittest discover -s prototype/recovery-boundary/tests`
   and `python -B prototype/recovery-boundary/verify_gate_a.py`;
3. run actionlint over the protected Test workflow and syntax-check all three
   inert `*.tmpl` files without making them discoverable;
4. build all four prototype Dockerfiles by exact path with pull and no-cache;
5. scan all four resulting images independently with unsuppressed
   HIGH/CRITICAL vulnerability policy and nonzero exit on a finding; and
6. require every preceding job and one aggregate Gate-B check to succeed.

The protected `Test` workflow implements these steps and preserves its required
`test` context as the fail-closed aggregate. Only a successful exact-head run is
prototype authority; a green unrelated check is not.

## Hard exclusions

No production identifier, data, credential, domain, route, firewall, environment, package, workflow, capsule, plan, recovery evidence, apply, canary, rollback, or deployment may be read, copied, selected, or changed. A Gate-C pass is substrate evidence only and cannot authorize any later gate.
