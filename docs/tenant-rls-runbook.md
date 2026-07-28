# ReReply PostgreSQL Tenant RLS Runbook

This runbook enables PostgreSQL Row-Level Security (RLS) for ReReply's
WhatsApp CRM data. Apply it to staging first. Do not change the production
database until the feature branch is merged, all required checks are green,
and the staging isolation checks below pass.

## Isolation model

- The web service and campaign workers connect as a restricted
  `rereply_app` database role.
- Every authenticated request, Meta webhook, and background job sets
  `app.current_organization_id` inside a transaction.
- PostgreSQL policies permit CRM rows only when their `organization_id`
  matches that transaction-local setting.
- Child tables without an `organization_id` derive their tenant from their
  protected parent.
- A separate migration connection owns the schema and installs policies.
- Server and worker startup fail closed if the configured runtime connection
  is the owner, a superuser, has `BYPASSRLS`, or is missing a policy.

Identity/control-plane tables and the long-lived WhatsApp Calling subsystem
retain their existing application-level organization checks in this first
phase. The RLS policy set covers contacts, messages, WhatsApp accounts,
templates, campaigns, chatbot data, transfers, audit logs, catalogs,
conversation notes, webhooks, tags, and dashboard data.

## Required secrets and variables

Create a dedicated PostgreSQL user named `rereply_app` in the DigitalOcean
database. It must be:

```text
NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS
```

Use different credentials for staging and production. Store URLs as encrypted
DigitalOcean variables; never commit them.

The server and worker components require:

```text
WHATOMATE_DATABASE__URL=<private/direct URL for rereply_app>
WHATOMATE_DATABASE__RUNTIME_ROLE=rereply_app
WHATOMATE_DATABASE__RLS_ENABLED=true
```

The migration job additionally requires:

```text
WHATOMATE_DATABASE__MIGRATION_URL=<private/direct schema-owner URL>
```

Prefer a dedicated deployment job for the owner URL so the long-running web
and worker processes do not receive owner credentials. If the platform can
only run a component-level pre-deploy command, keep the variable encrypted,
restrict who can edit the app, and remove it from the runtime component after
the migration.

Use a direct PostgreSQL connection for RLS transactions. Do not put the
runtime URL through a session-mode proxy that can retain session settings
between clients.

## Staging rollout

1. Take a staging database backup and record the restore identifier.
2. Create the restricted `rereply_app` staging database user.
3. Deploy the candidate image with the service command:

   ```text
   ./rereply server -config config.toml
   ```

   Do not include `-migrate`.

4. Before starting the new service revision, run:

   ```text
   ./rereply rls-migrate -config config.toml
   ```

   DigitalOcean production must keep `rereply-rls-migrate` as a
   `PRE_DEPLOY` job sourced from the same protected `main` commit as the web
   service. The runtime now verifies a versioned routing-function marker, so
   an old resolver body causes startup to fail closed instead of silently
   leaving new queue states undiscoverable.

5. Start the service and confirm the log contains:

   ```text
   PostgreSQL tenant RLS verified
   ```

6. Start a worker revision with the same restricted runtime URL and confirm
   the same verification log.
7. Run the automated isolation test:

   ```bash
   go test -v ./internal/database -run '^TestTenantRLS_'
   ```

8. Create two disposable staging organizations and verify:

   - Organization A cannot list, open, update, or delete Organization B's
     contacts or messages.
   - A superadministrator can switch deliberately to either organization but
     sees only the selected organization's CRM data.
   - An inbound WhatsApp message is routed to the account's organization.
   - A template status webhook updates only accounts belonging to its WABA.
   - A campaign worker creates messages and contacts only in the job's
     organization.
   - Logging out and back in does not retain a previous organization context.

9. Observe database errors, HTTP 5xx rate, webhook failures, queue depth, and
   PostgreSQL connection usage for at least one normal business cycle.

## Production rollout

Repeat the staging sequence during a low-traffic maintenance window. Do not
reuse staging credentials. Keep the previous application revision available
until the smoke tests pass.

The migration is idempotent: it recreates ReReply's named policies, grants the
runtime role access, and verifies that role cannot bypass RLS.

## Rollback

Use rollback only for an incident confirmed to be caused by RLS:

1. Stop or scale down web and worker components to prevent mixed policy modes.
2. With the owner migration connection, run:

   ```text
   ./rereply rls-migrate -config config.toml -rollback
   ```

3. Set `WHATOMATE_DATABASE__RLS_ENABLED=false`.
4. Redeploy the previous known-good application revision.
5. Verify contacts, messages, webhooks, and workers.

The rollback removes only ReReply's RLS policies and resolver functions. It
does not delete tenant data.

## Ongoing controls

- Make the GitHub `tenant-isolation`, `test`, `lint`, `build`, and `security`
  checks required on `main`.
- Run all schema changes through the migration job, never through
  `server -migrate` in RLS mode.
- Add every new table containing `organization_id` to
  `DirectTenantTables`, or define a parent-derived expression in
  `RelatedTenantTables`.
- Add an isolation test for each new child table or new background processor.
- Rotate both database credentials after any suspected exposure.
