# Repository CRM canary

These are the existing 13 CRM canary checks, in their original execution order,
with the original selectors, assertions, timeouts and late-layout probes. The
contract test compares the port to `frontend/canary-driver/runner.mjs`. That
production driver stays intact until Part A PR11.

The transport is synthetic: the Graph stub delivers signed inbound webhooks,
accepts outbound Graph requests, and proves both through its authenticated
journal. Denial checks also prove that no send reached the stub. Browser traffic
is restricted to the selected product origin. Product authorization, tenant RLS,
manual plan entitlements, WebSockets, read cursors and mirrors run normally.
Every scenario re-registers its saved synthetic account after a stub redeploy;
missing or restarted journal evidence during a check fails the proof.

## Local/CI

Use an empty, disposable PG17 instance (the digest in `canary-local.yml`) and
Redis7. Set an absolute `RUNNER_TEMP` outside the checkout. Optional port variables
are `CANARY_PG_PORT`, `CANARY_REDIS_PORT`, `CANARY_BACKEND_PORT`, and
`CANARY_STUB_PORT`; all connections use loopback. The bootstrap PostgreSQL user
is `postgres`, with `CANARY_PG_PASSWORD` (default `postgres` for the CI service).

From the repository root:

```sh
export CANARY_PROFILE=local
export CANARY_PRIVATE_FILE="$RUNNER_TEMP/canary/private.json"
export CANARY_REPORT_FILE="$RUNNER_TEMP/canary/report.json"
(cd frontend && npm ci --ignore-scripts && npm run test:canary:unit)
node frontend/e2e/canary/local-stack.mjs configure
node frontend/e2e/canary/local-stack.mjs bootstrap
(cd frontend && npm run build)
cp -r frontend/dist/. internal/frontend/dist/
go build -mod=readonly -o "$RUNNER_TEMP/canary/rereply" ./cmd/whatomate
go build -mod=readonly -o "$RUNNER_TEMP/canary/graph-stub" ./release/staging/graphstub/cmd/graph-stub
node frontend/e2e/canary/local-stack.mjs start
(cd frontend && npx playwright install chromium --with-deps)
(cd frontend && npm run canary:provision && npm run test:canary)
(cd frontend && npm run test:canary:verify -- "$CANARY_REPORT_FILE")
```

On Windows, build the two binaries with `.exe` suffixes. The launcher uses hidden
windows. `processes.json` records the process IDs; stop those processes and remove
only this run's disposable containers after use. Private logs stay beside it.
The wrapper invokes `go run ./release/staging/bootstrap`. It refuses ambient
`WHATOMATE_*`, `STUB_*`, `META_RELAY_*` and `GMAIL_RELAY_*` variables before any
operation and strips them again from child environments. Configuration refuses existing database roles, so rerunning `configure` requires
a new disposable PostgreSQL instance. It never drops a database or replaces roles.

The advisory workflow performs these steps on push and pull requests. It has
read-only repository permission, no GitHub environment, no secrets and no
uploaded artifacts. The application, database roles and stub get random secrets;
the workflow masks them. It is deliberately not a required branch-protection job.

## Staging

Use the private JSON file produced by `release/staging/setup.py app` (Part A PR8),
in an owner-only directory outside every checkout, without symlinks. Require
mode `0600` for the file and `0700` for its directory on Unix; on Windows the
current user must own them and be the only allowed principal. Set `CANARY_PROFILE=staging` and
`CANARY_PRIVATE_FILE=/absolute/path/to/private.json`. The saved `origin_sha256`
must equal SHA-256 of the exact canonical HTTPS origin. The Graph control path is
`<origin>/_stub/_control/...`; ingress strips only `/_stub`.

The setup file supplies `canary.origin`, `namespace`, `stub_origin`,
`stub_control_key`, `stub_access_token`, `stub_app_secret`, `stub_app_id`,
`admin_email` and `admin_password`. `npm run canary:provision` uses only product
and stub APIs, writes `canary.klinik_organization_id` and `canary.fixture` back to
that same private file, and preserves other fields. It needs no database access.
Then run `setup.py redeploy` to enable the exact synthetic Klinik organization in
the legacy-reply allowlist, before running the checks. Provisioning creates only
synthetic users, contacts, account, history, and a zero-priced manual entitlement
plan in this staging application; it never changes a provider subscription.

Both profiles refuse production origins and the `rereply-canary` namespace.
Local requires loopback. Staging also requires the exact origin hash and same
origin stub. Individual `CANARY_ORIGIN`, `CANARY_NAMESPACE`, `CANARY_STUB_ORIGIN`,
`CANARY_STUB_CONTROL_KEY`, `CANARY_STUB_ACCESS_TOKEN`, `CANARY_STUB_APP_SECRET`,
`CANARY_STUB_APP_ID`, `CANARY_ADMIN_EMAIL`, and `CANARY_ADMIN_PASSWORD` variables
can supply local values; staging still checks the setup file's origin hash.

Provision only once per fresh namespace. On partial failure, inspect the private
logs and use a new synthetic namespace; the tool does not delete data to retry.
Existing fixtures are reused for check and stress runs. For staging releases,
the owner installs the allowlisted fixture export as `STAGING_CANARY_FIXTURE_JSON`
alongside `STAGING_ORIGIN` and `STAGING_STUB_CONTROL_KEY`, as described in the
[staging release contract](../../../docs/staging-release-contract.md). The full
setup state stays private; credentials and fixture descriptors never enter
GitHub outputs or uploaded artifacts.

## Stress proof and reports

`CANARY_STRESS=1 npm run test:canary` runs all 13 checks 120 times, then each
late-layout check another 80 times. Set `CANARY_STRESS=1` for the verifier too.
The workflow's manual `stress` input performs the same run. Retries are always
zero and tests are serial. A normal report must have exactly 13 passed names;
a stress report must have exactly 1,720 passes in the specified inventory.
Any failure, global error, skip, retry or missing/duplicate normal check fails.

The private reporter gives one line of name/status progress. The raw Playwright
line reporter is intentionally replaced because failed password fills and
authenticated requests can expose secrets in its call logs. The private machine
report also contains only names and statuses, without bodies, configuration or
credentials. Trace, screenshot, video and automatic error-context snapshots are
disabled. Disposable Playwright output uses a separate private subdirectory
outside the checkout with output preservation disabled; it never shares the
setup directory that holds credentials. CI uploads no reports or artifacts.
