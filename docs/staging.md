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
   ```

   Create the dedicated directory with access restricted to your own Windows
   user before initializing doctl; keep both the directory and config file
   private. Enter the token at doctl's prompt. The file must contain only the
   `rereply-staging` auth context, selected as `current-context`; an ambient
   DigitalOcean token/context or a global `access-token` is refused. The setup
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

The public output contains only completion codes and identity hashes. The origin,
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
| WhatsApp | Graph calls go to `http://graph-stub:8090`; credentials and account IDs are synthetic |
| Meta relay | Staging environment; Facebook and Instagram Graph bases point to the stub; registry off; one inert synthetic static mapping points to loopback port 9 because the relay refuses an empty mapping |
| Gmail relay | TLS Valkey, synthetic mailbox/OAuth placeholders; auth/token/API bases point to `http://127.0.0.1:9` |
| Other integrations | Managed Messenger, Instagram, Threads and Threads review gates off; configurable Qwen/Search Console/Meta onboarding URLs point to port 9; no real client keys |
| Voice | Piper/model/encoder paths disabled; calling recording disabled |
| Graph stub | Internal port 8090; only public `/_stub/_control/...` routes to the stub, rewritten to its HMAC-protected `/_control/...` API; Graph routes stay internal |

There is no universal environment switch for every tenant AI/SSO feature. The
fresh synthetic database has those features unconfigured; the kit does not claim
network-level egress isolation. Do not configure real integrations or enable AI,
SSO, TTS or calling in this instance. The internal stub accepts only synthetic
credentials and refuses known production domains.

## Later staging pipeline setup

Create GitHub environments `staging` and `staging-e2e`, restricted to `main`.
PR9/10 will consume these; this kit does not create or populate them.

| Environment | Planned values |
|---|---|
| `staging` | `STAGING_DO_TOKEN`, `STAGING_TARGET_JSON` |
| `staging-e2e` | `STAGING_ORIGIN`, `STAGING_ADMIN_EMAIL`, `STAGING_ADMIN_PASSWORD`, `STAGING_AGENT_PASSWORD`, `STAGING_KLINIK_ORG_ID`, `STAGING_OTHER_ORG_ID`, `STAGING_STUB_CONTROL_KEY` |

Keep all values private. Create a separate expiring staging deploy token with
only the scopes needed by that later lane, then revoke the short-lived setup
token and remove its context from the dedicated config. Nothing here installs a
production token or changes the production release approval policy.

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

For an intentional staging reset, first stop staging releases. In the staging
team only, destroy the staging app if recreating it, add your IP to both clusters,
and drop/recreate the empty `rereply` database under doadmin. Archive the old
private state, run `db` and `app` with a new private file in a fresh empty
directory named `rereply-staging` outside all Git checkouts, provision synthetic
fixtures, then redeploy. Never perform these steps on production. A new app has
a new identity/origin; refresh the later pipeline target only through its reviewed
configuration path.

Reference: [DigitalOcean app specification](https://docs.digitalocean.com/products/app-platform/reference/app-spec/),
[internal routing](https://docs.digitalocean.com/products/app-platform/how-to/manage-internal-routing/),
and [team context in the account API](https://docs.digitalocean.com/reference/api/reference/account/).
