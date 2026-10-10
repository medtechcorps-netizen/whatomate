# Staging setup (Part A PR8)

This kit creates an isolated, synthetic ReReply instance in the **ReReply
Staging DigitalOcean team**. The similarly named project in the production
team is not the destination. The kit has no production write path. No production
row, dump, connection string, Meta token, Google credential or Qwen key belongs
in staging or CI.

The repository now contains the preparation tooling. Running it creates a paid
app and changes staging database trusted sources. Run these commands only after
the owner authorizes staging setup, from a separate Windows Terminal window.
No provisioning happens when these files are merged or their unit tests run.

## Prepare the team and resources

1. Reconnect the existing **ReReply Staging team** created during the earlier
   setup. Do not recreate it or use the similarly named production-team project.
   Verify its billing and billing alert. Inspect existing resources before
   creating anything; the following names must identify staging resources only.
   If absent, create a Singapore VPC,
   PostgreSQL 17 cluster `rereply-staging-pg`, and managed Valkey cluster
   `rereply-staging-valkey`, both in that VPC. Choose the smallest suitable sizes.
   Review current prices before purchase. The earlier plan estimated roughly
   US$55–60/month for the app plus the two databases; this is a budget estimate,
   not a quote or a spending authorization.
2. Immediately add your current IP to **both** clusters' trusted sources. Do
   not leave either list empty. In PostgreSQL's console create database `rereply`
   and user `rereply_app`. The migration owner is the existing `doadmin`; do not
   create an owner role with SQL. The bootstrap checks the real role shape before
   writing anything.
3. Create a short-lived setup token in that team. Keep it out of chat and the
   checkout. Use a dedicated doctl configuration containing exactly one context:

   ```powershell
   doctl --config "$env:USERPROFILE\rereply-staging\doctl.yaml" auth init --context rereply-staging
   doctl --config "$env:USERPROFILE\rereply-staging\doctl.yaml" auth switch --context rereply-staging
   ```

   Create the dedicated directory with access restricted to your own Windows
   user before initializing doctl; keep both the directory and config file
   private. Enter the token at doctl's prompt. The file must contain only the
   `rereply-staging` auth context, selected by the real doctl `context` key;
   doctl ignores `current-context`. `auth init` can leave `context: default`,
   so the explicit `auth switch` is required. Both commands also serialize
   command defaults into the config; normalize them as described below before
   any setup command. An ambient DigitalOcean token/context, another endpoint,
   multiple auth contexts or a nonempty global `access-token` is refused. The setup
   token needs account/inventory reads, database connection credentials,
   staging app create/update and database firewall update permissions. It is
   separate from the later staging deployment token. Do not alter the personal
   or production doctl contexts.
4. Copy `release/staging/target.example.json` outside the checkout. Fill in the
   exact PostgreSQL, Valkey and VPC IDs and `team_sha256`, the SHA-256 of the
   account API's **team.uuid** UTF-8 value. It is an opaque identifier, not
   necessarily an RFC UUID. Confirm the team identity in the console before
   saving its hash. The private state pins this first identity for every later
   command; Part A PR9 will pin the reviewed team hash in its deployment target.
   The kit also refuses a credential that can see the production app or cluster.

5. From the reviewed checkout, normalize **only this dedicated staging config**
   using the team hash confirmed above. This command performs one read-only
   account lookup and a local atomic config replacement; it creates no provider
   resources and needs no image publication or database. It can therefore run
   before provisioning. Replace the hash placeholder; it is not a credential.

   ```powershell
   py -3 release/staging/setup.py normalize-config --doctl-config "$env:USERPROFILE\rereply-staging\doctl.yaml" --team-sha256 CONFIRMED_STAGING_TEAM_SHA256
   ```

   The file must be named `doctl.yaml` in an owner-only directory named
   `rereply-staging`, outside every checkout. The normalizer checks the directory
   and file ACLs, requires the selected single staging context, and discards all
   command defaults. The exact `auth-contexts.default: "true"` display sentinel
   that doctl 1.164's switch can serialize is discarded only with an empty global
   token and the staging context selected; any second credential is refused.
   It verifies the expected active **ReReply Staging** team
   using a protected temporary minimal config and the fixed official API endpoint,
   then atomically replaces the original after checking for changes detected
   during validation. Do not run `auth init`, `auth switch`, or another writer
   concurrently: the comparison is not an atomic compare-and-swap against an
   uncooperative writer. Refusal does not replace the original and removes the
   temporary file. Output
   contains only a fixed completion/refusal code, never a token or account data.
   Do not use it on a personal/production config. Do not hand-edit guards or copy
   tokens to a second file. Running `auth init` or `auth switch` again expands the
   file, so rerun normalization afterwards. Ordinary `db`, `app` and `redeploy`
   continue to reject expanded command-default files.

   Keep this auth/config directory separate from the first bootstrap's **empty**
   state directory, for example `C:\private\rereply-staging\state.json` below.
   Putting `doctl.yaml` or the external JSON inputs in the state directory makes
   the first `db` command refuse it as nonempty.

## Publish staging images

After this PR is merged and the owner authorizes one build, dispatch
**Staging images** on `main`. The workflow also builds after main changes to its
bootstrap/stub sources, Dockerfiles or Go dependency files. It has no deployment
environment or stored secret; its ephemeral workflow token publishes only the
two staging image packages and attestations.

Make `rereply-staging-graph-stub` and `rereply-staging-bootstrap` packages public.
Copy the two immutable references and source SHA from the successful run into an
external copy of `release/staging/images.example.json`. Tags are not accepted.
The kit verifies the exact `staging-images.yml@refs/heads/main` signing certificate,
source digest and hosted runner, using the same certificate checker as releases.
The bootstrap image runs as UID 65532 from scratch. No SBOM or vulnerability
exception is implied by the staging-only publication.

Product images come from the fully verified, attested production **release
record**, through read-only GitHub access; no production provider credential or
app/DB request is used. They must contain the nonproduction Graph-base support.
The schema guard must find no difference between that release and the bootstrap
source. A setup already in progress keeps its verified record pinned while
normal production releases continue.

## Bootstrap and create the app

Use a clean LF checkout at the current `origin/main`, with Python 3.11+, doctl,
Docker and authenticated gh available. The tool checks the live GitHub main SHA
as well as the local ref. Review the two external JSON inputs before running.

```powershell
py release/staging/setup.py db --target C:\private\staging-target.json --images C:\private\staging-images.json --private-file C:\private\rereply-staging\state.json --operator-ip YOUR_CURRENT_IP
py release/staging/setup.py app --target C:\private\staging-target.json --images C:\private\staging-images.json --private-file C:\private\rereply-staging\state.json
```

`db` makes no DigitalOcean API writes. It verifies the role/empty-schema
preconditions through the staging bootstrap image, creates synthetic seed data,
then runs the selected production web image's `rls-migrate` in verify-only mode.
Both containers receive the private config on stdin, never command-line
arguments, environment variables or an exposed bind mount. Child output is
captured; failures print fixed reason codes. The dedicated private directory has
its Windows ACL replaced with an owner-only grant (or mode 0700 on POSIX) before
secret state is written. Existing state and its directory must still be owned by
the current user and permit no other user; unexpected grants stop the command.
Use a new, empty directory named `rereply-staging` for the first bootstrap.
Save its contents securely and do not commit them.

The bootstrap's `ENTRYPOINT ["/bootstrap"]` receives `-config /dev/stdin` as
arguments. The web image's `ENTRYPOINT ["./rereply"]` receives
`rls-migrate -config /dev/stdin`; Docker must not be passed a second `./rereply`.
The App Platform PRE_DEPLOY `run_command` is the complete command
`./rereply rls-migrate -config config.toml`, matching the production job contract.
App Platform's [run command overrides the image entrypoint](https://docs.digitalocean.com/products/app-platform/how-to/deploy-from-container-images/).
Runtime environment overrides supply its private URLs. A local scratch-image
probe verified `os.ReadFile("/dev/stdin")` with UID 65532 and piped config bytes.

`app` creates `rereply-stage` with digest-only images, the staging VPC, and
`rereply-rls-migrate` as its PRE_DEPLOY job. Owner credentials are scoped to that
job; web services receive only the runtime URL. It adds the app to each cluster's
nonempty trusted sources, observes the exact returned deployment, requires its
migration job to succeed with the selected web digest, and checks all six product
health endpoints. Both the app and the actual deployment must also retain the
expected images, components, ingress, database bindings and commands. Only then
does it replace each cluster's rules with exactly
the app and verify the readback. Your PC loses database access at that point.

The public output contains only completion codes and identity hashes. Successful
`app` and `redeploy` print `team_sha256`, `app_id_sha256` and `origin_sha256`.
PR9's reviewed `ship-target-staging.json` uses the first two fingerprints, mapping
`team_sha256` to its `team_uuid_sha256` field; raw resource IDs stay private.
The origin,
generated synthetic administrator and canary/stub credentials are in the private
file. Provision the canaries using Part A PR7's REST-only command:

```powershell
$env:CANARY_PROFILE = 'staging'
$env:CANARY_PRIVATE_FILE = 'C:\private\rereply-staging\state.json'
npm --prefix frontend run canary:provision
py release/staging/setup.py redeploy --target C:\private\staging-target.json --images C:\private\staging-images.json --private-file C:\private\rereply-staging\state.json
```

The initial app has the legacy reply gate disabled. Provisioning records the
synthetic administrator's organization as `canary.klinik_organization_id`;
`redeploy` enables the gate for exactly that canonical UUID. It refuses unrelated
non-secret environment, image or topology drift in the current app and active
deployment, then restores both firewalls to exactly the app.
PR7 may be developed in parallel; do not run its command until it is merged.

## Configuration boundary

| Component | Connection and external behavior |
|---|---|
| Web | `app.environment=staging`, RLS on, runtime PostgreSQL URL, TLS Valkey, synthetic encryption/JWT/admin credentials |
| WhatsApp | Graph calls go to `http://graph-stub`; credentials and account IDs are synthetic |
| Meta relay | Staging environment; Facebook and Instagram Graph bases point to the stub; registry off; one inert synthetic static mapping points to loopback port 9 because the relay refuses an empty mapping |
| Gmail relay | TLS Valkey, synthetic mailbox/OAuth placeholders; auth/token/API bases point to `http://127.0.0.1:9` |
| Other integrations | Managed Messenger, Instagram, Threads and Threads review gates off; configurable Qwen/Search Console/Meta onboarding URLs point to port 9; no real client keys |
| Voice | Piper/model/encoder paths disabled; calling recording disabled |
| Graph stub | HTTP listener 8090; only public `/_stub/_control/...` routes to the stub, rewritten to its HMAC-protected `/_control/...` API; Graph routes stay internal |

Internal requests use App Platform's default service-name LAN routes:
`http://graph-stub` for Graph calls and `http://omnitech-web` for stub callbacks.
These routes use port 80 and forward to each service's main `http_port`; the
application listeners remain 8080, 8081, 8082 and 8090. The template declares no
additional `internal_ports`, because duplicating `http_port` there is rejected.
Provider readback may omit the empty list, but any nonempty or malformed value
is refused. See [DigitalOcean's internal routing documentation](https://docs.digitalocean.com/products/app-platform/how-to/manage-internal-routing/).
Only the public `/_stub/_control` prefix routes to the stub's HMAC-protected
control API. Its rewrite is `/_control/`, including the trailing slash, so the
remaining endpoint name is separated from the control prefix. For example,
`/_stub/_control/accounts` must reach `/_control/accounts`, the path signed by
the control client. See [DigitalOcean's rewrite specification](https://github.com/digitalocean/godo/blob/main/apps.gen.go).

There is no universal environment switch for every tenant AI/SSO feature. The
fresh synthetic database has those features unconfigured; the kit does not claim
network-level egress isolation. Do not configure real integrations or enable AI,
SSO, TTS or calling in this instance. The internal stub accepts only synthetic
credentials and refuses known production domains.

## Staging pipeline setup

Create GitHub environments `staging` and `staging-e2e`, restricted to `main`.
The staging workflow consumes the following environment secrets; this kit does
not create or populate them.

| Environment | Secret | Source |
|---|---|---|
| `staging` | `STAGING_DO_TOKEN` | Separate expiring staging deployment token |
| `staging` | `STAGING_TARGET_JSON` | Exact contents of `target-export.json` |
| `staging-e2e` | `STAGING_ORIGIN` | Verified canonical `canary.origin` from successful setup |
| `staging-e2e` | `STAGING_STUB_CONTROL_KEY` | Existing private `canary.stub_control_key` |
| `staging-e2e` | `STAGING_CANARY_FIXTURE_JSON` | Exact contents of `canary-export.json` |

After fixture provisioning and successful allowlist redeployment, run the offline
exporters in a fresh shell without `CANARY_*` or application configuration
overrides. Use the existing owner-only setup directory outside the checkout:

```powershell
$stagingPrivateDir = "$env:USERPROFILE\rereply-staging-state\rereply-staging"
py -3 -I -S -B release/deployment/stage_fixture.py export-fixture --private-file "$stagingPrivateDir\state.json" --output "$stagingPrivateDir\canary-export.json"
py -3 -I -S -B release/deployment/stage_fixture.py export-target --private-file "$stagingPrivateDir\state.json" --output "$stagingPrivateDir\target-export.json"
```

Both commands require completed setup with the saved allowlist applied, refuse
existing outputs, and write new owner-only files beside the state. They neither
contact a provider nor set GitHub secrets. Target export additionally requires
the independently reviewed real team/app fingerprints committed in
`release/deployment/ship-target-staging.json`; null placeholders remain blocked.

The fixture export preserves the two non-superuser logins, exact fixture
identities, namespace, origin and synthetic stub app secret. It excludes the
bootstrap administrator, database/Valkey credentials and unrelated setup state.
The target export contains reviewed resource/image/template bindings without
canary logins or app/database secrets. Never upload the full state file, copy it
into a GitHub secret, or publish either export as an artifact. Bootstrap admin
credentials and separate organization/password inputs are not E2E secrets.

The E2E job masks its three inputs before checkout and runs
`stage_fixture.py import-fixture --output "$RUNNER_TEMP/rereply-staging/canary.json"`.
The importer binds them to the deployment receipt, candidate, run, pinned app and
origin before writing a fresh private directory. The workflow then removes the
three `STAGING_*` variables and passes the imported file to the CRM canary. CI
reuses the saved fixture; it does not provision one. Provider credentials are
confined to staging deploy/rollback steps and never enter the E2E job. See the
[staging release contract](staging-release-contract.md) for exact schemas and
receipt/report bindings.

Use a separate deployment PAT in the **ReReply Staging** team with an explicit
expiry and exactly these scopes: `account:read`, `app:read`, `app:update`,
`database:read`, `project:read`, `vpc:read`, `actions:read`, `regions:read`, and
`sizes:read`. The final three are DigitalOcean's required read dependencies for
[app updates](https://docs.digitalocean.com/reference/api/scopes/app/update/)
and [database reads](https://docs.digitalocean.com/reference/api/scopes/database/read/).
Keep `vpc:read` even though this lane does not call the VPC endpoint: Claude's
2026-09-30 provider comparison proved that a token without it receives an app
spec with `vpc` omitted. The existing VPC therefore still needs read permission;
the lane must refuse an incomplete spec. `project:read` preserves the accepted
staging plan and is an associated permission for app updates; the earlier
multi-scope recovery did not isolate whether it was independently necessary.
The lane reads existing database/firewall metadata and updates the pinned app;
it does not create resources or change firewalls. After setup and the reviewed
deployment-token handoff are complete, revoke only the short-lived staging setup
token and remove its dedicated context. Keep existing production DO/GitHub
tokens unchanged; production release approval remains in force.

## Failure and reset

A `pending_operation` is written before bootstrap or a provider mutation.
Timeouts and ambiguous results are not retried. Stop, inspect only the selected
staging app/deployment/database in the console, and reconcile the private journal
with what actually happened before taking another action. Do not delete the
journal to retry an uncertain app creation. The tool never deletes resources,
restores databases, runs SQL role creation or automatically rolls back.

For read-only reconciliation, retain the private state and use the explicit
staging configuration throughout. This example prints only resource identity and
status; never print a whole app spec, connection response or the private state.

```powershell
$doArgs = @('--config', "$env:USERPROFILE\rereply-staging\doctl.yaml", '--context', 'rereply-staging', '--api-url', 'https://api.digitalocean.com', '--http-retry-max', '0', '--interactive=false', '--output', 'json')
doctl @doArgs apps list | ConvertFrom-Json | Select-Object id, @{Name='name';Expression={$_.spec.name}}
# Read APP_ID and DEPLOYMENT_ID from the private journal locally, then replace
# these placeholders. If either is absent, find it in the staging console first.
$app = doctl @doArgs apps get APP_ID | ConvertFrom-Json
$app | Select-Object id, @{Name='active';Expression={$_.active_deployment.id}}, @{Name='pending';Expression={$_.pending_deployment.id}}, @{Name='in_progress';Expression={$_.in_progress_deployment.id}}
$deployment = doctl @doArgs apps get-deployment APP_ID DEPLOYMENT_ID | ConvertFrom-Json
$deployment | Select-Object id, phase
$deployment.jobs | Select-Object name, phase
doctl @doArgs databases firewalls list POSTGRES_ID | ConvertFrom-Json | Select-Object type, value
doctl @doArgs databases firewalls list VALKEY_ID | ConvertFrom-Json | Select-Object type, value
```

Record the resulting identities and status privately, with the failed operation
and timestamp. Use these cases to decide the reviewed recovery action:

| Pending operation | Evidence needed before any further write |
|---|---|
| `bootstrap` | The console's staging database/roles and captured private local failure context. A partly bootstrapped database must not be bootstrapped again blindly; use an authorized staging reset if its exact state cannot be proved. |
| `create-app` with no app ID | Search this staging team's app inventory and deployment history. A lost response may still have created the app. Confirm whether zero or exactly one matching app exists; multiple candidates require manual review. |
| `create-app`/`redeploy` with IDs | Inspect that exact deployment, its migration job, its full spec privately, and all six health endpoints. Confirm no competing deployment and compare image digests, ingress, DB bindings and non-secret envs to the template plus pinned private inputs. |
| Firewall interruption | Read both rule lists. One may already contain only the app while the other retains the operator IP. Do not open either list or assume both writes completed. |

This kit deliberately has no automatic journal-clear/resume command. Reconcile
the evidence with the maintainer, who can prepare a narrowly scoped recovery
using the existing app/deployment IDs. Clear a pending marker only as part of that
reviewed recovery after all postconditions and the private origin/allowlist state
are established. A failed or uncertain deployment is not proof that an earlier
create/update did nothing. Do not rerun `app` against an already-created app.

### Reset the synthetic database while retaining the app

`release/staging/reset.py` provides the retained-app reset recipe. This is an
owner-operated destructive maintenance procedure, not routine release recovery.
It keeps the app ID, origin, clusters, VPC, verified setup image set and coherent
credentials. The existing `setup.py db/app/redeploy` interfaces and guards are
unchanged. The adapter does not delete an app/database, archive or restore the
app, create roles, flush Valkey, rotate credentials, or change GitHub settings.

The initial supported state is a healthy, fully provisioned app on the **saved
verified production-record image baseline**. An app left on another staging
candidate is refused; this command never silently adopts or downgrades it.
Any separately needed restoration must be reviewed first. The checkout must be
clean current `main`, its required Test/E2E workflows successful, and its support
images attested and schema-compatible. No live reset rehearsal is implied by
the synthetic tests.

Before starting, the owner establishes an exclusive maintenance window covering
all Release dispatches/reruns/approvals, direct canary/provision commands, setup,
and console writers. Keep it until fresh exports and the staging proof handoff
are complete. The adapter checks Release idleness and owns a local lock, but
neither is a universal lock on those other callers. `pending_operation` blocks
ordinary setup and exports; direct frontend canaries and already-exported hosted
inputs do not consult it.

Use the existing owner-only files outside every Git checkout. Each file's parent
must be named `rereply-staging`. Choose one fresh UUID for the operation and keep
it for every phase; never choose a new ID merely to retry a failed phase:

```powershell
$resetID = [guid]::NewGuid().ToString()
$resetArgs = @('--doctl-config', $doctlConfig, '--target', $targetFile,
  '--images', $imagesFile, '--private-file', $privateFile, '--reset-id', $resetID)
py -3 release/staging/reset.py prepare @resetArgs --confirm-maintenance-window
```

`prepare` checks exact staging pins, inventory, baseline topology/images, stable
app/active specs including SECRET values, migration and six health endpoints.
It creates private `reset-<UUID>-original.json`, `reset-<UUID>-working.json` and
`reset-<UUID>.json` beside the canonical state, then marks the canonical state
pending. The original backup is never overwritten. The working copy keeps the
old *applied* allowlist for the existing redeploy before-spec check, while clearing
the old fixture/organization and choosing a new synthetic namespace.

The following are **explicit owner steps**, outside the adapter:

1. In the staging team, archive this same app using its complete unchanged spec
   plus `maintenance.archive: true` (and `enabled: true` if serialized). Wait for
   the provider to finish stopping its consumers/jobs. Confirm that no owned
   application/relay process or DB writer can restart during recreation. An
   offline page alone does not prove quiescence. Do not add an offline URL or
   change any other spec leaf; unknown archive shapes are refused.
2. Confirm that only disposable synthetic data is present. Recreate only the
   database named `rereply` under the existing `doadmin` in the exact retained
   staging PostgreSQL cluster, preserving `doadmin`/`rereply_app` and credentials.
   The bootstrap still independently enforces its empty-schema, role, membership
   and ownership checks. There is no generated SQL destruction command here.
3. Review the dedicated staging Valkey consumer scope while consumers are stopped.
   PostgreSQL recreation does not clear queued work or sessions: campaign streams
   (`whatomate:campaigns`), web refresh/session/cache data, and fixed relay prefixes
   (`rereply:meta-relay:` and `rereply:gmail-relay:`, including its `queue:` suffix)
   are not limited to the canary namespace. Establish that the exact owned scope
   is empty, or perform a separately authorized scoped cleanup. Do not flush a
   shared logical DB or infer authority for `FLUSHALL`. The confirmation below is
   an operator assertion; the adapter does not enumerate every consumer/key.
4. Temporarily make each cluster's trusted-source list exactly the pinned
   operator IP, never an empty list. The adapter re-reads both lists and privately
   re-reads DB/Valkey credentials; changed credentials require separate review.

```powershell
py -3 release/staging/reset.py db @resetArgs --operator-ip $operatorIP `
  --confirm-quiesced --confirm-empty-database --confirm-owned-valkey-clean
```

This phase verifies the retained archived app's exact identity and full spec,
then runs the same attested bootstrap and selected web `rls-migrate` verifier as
ordinary `db`, with private configuration only on stdin. A partial bootstrap or
unknown subprocess result remains pending and must not be repeated.

While the app is still archived, replace **both** cluster trusted-source lists
with exactly this retained app (remove the temporary operator IP). This restores
the app's DB/Valkey connectivity before its migration starts; operator-only
firewalls would prevent PRE_DEPLOY from succeeding. The redeploy adapter checks
these exact app-only lists before issuing an update.

Restore the **same app** through the console using its exact saved baseline spec
with the maintenance object removed. Wait for its migration and ACTIVE state.
No other config/image/credential changes are accepted. Then run these phases
once in order, checking each exit code before continuing:

```powershell
py -3 release/staging/reset.py redeploy @resetArgs
py -3 release/staging/reset.py provision @resetArgs
py -3 release/staging/reset.py redeploy @resetArgs
py -3 release/staging/reset.py verify @resetArgs
py -3 release/staging/reset.py finalize @resetArgs
```

The first redeploy uses unchanged `Setup.app` to disable the old allowlist and
reverify app-only firewalls. A reset-specific wrapper compares the full private
spec and owned deployment again immediately before the single update call. Only
the first redeploy permits the owner's restore to have changed the deployment
ID; the second must still own the exact deployment observed after the first.
Provision invokes the existing API fixture command
once against the working file and fresh namespace; it discovers the new
bootstrap organization and refuses reuse of the old organization. A partial
provisioning failure is not resumable by replay. The second redeploy applies only
the new organization's allowlist, verifies migration/health and closes both
firewalls to the retained app.

`verify` refuses an existing report, invokes the normal staging canary once with
an explicit working-file path and no drill, and requires all 13 unique checks to
pass once with zero retries. Its private journal binds that invocation to the
source, working fixture hash, namespace, origin and report hash. The raw reporter
file alone does not authenticate those bindings, and this local reset check is
not a hosted stage receipt or a promotion proof. Finalization rechecks source,
state, stable spec/migration, six health endpoints and app-only firewalls before
atomically publishing the new canonical private state. The final pending journal
records the exact intended state before publication.

After finalization, create fresh minimal fixture/target exports and update the
changed handoff values using the existing reviewed export procedure above. The
target's `canary_organization` changes; retaining app/origin/control-key identity
does not make the old fixture export usable. Keep the maintenance window until
that handoff and a fresh stage verification are ready. Old reports/receipts remain
historical records; this local operation does not cryptographically revoke them.

On **any** failure, retain all private files and the original backup. Do not
clear a pending marker, delete a lock after an unknown outcome, invoke an
already-consumed phase, overwrite a report, or run ordinary setup against the
working file. Reconcile exact app/deployment/spec/firewall state, bootstrap or
fixture partial effects, and the journal's intended payload read-only first.
Only a separately reviewed recovery may settle that operation. Even a final
publication failure can mean the new state was already written; never replay
remote mutations to repair a local acknowledgement.

DigitalOcean documents archive/restore as a complete same-app spec update; the
adapter only recognizes the documented archive flag and does not automate it.
See [archive and restore](https://docs.digitalocean.com/products/app-platform/how-to/archive-restore/).

Reference: [DigitalOcean app specification](https://docs.digitalocean.com/products/app-platform/reference/app-spec/),
[internal routing](https://docs.digitalocean.com/products/app-platform/how-to/manage-internal-routing/),
and [team context in the account API](https://docs.digitalocean.com/reference/api/reference/account/).
