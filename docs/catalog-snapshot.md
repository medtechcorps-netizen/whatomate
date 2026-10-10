# Read-only catalog snapshot

`catalog-snapshot` compares the public database structure with the reference
catalog compiled into the release. It does not run migrations, backfills,
application functions, or tenant queries. Run it only after the deployed
release includes the command:

```sh
./rereply catalog-snapshot -config config.toml -mode compare -seed-counts
```

The command uses the configured **runtime** database connection. Migration
credentials are never a fallback. It forces read-only startup settings and
collects metadata in one read-only, repeatable-read transaction with a
60-second limit. A failed capture prints a fixed error without partial output
or connection details.

## Reading the result

The default `compare` report contains:

- A SHA-256 for known public object identities and their attributes. A missing
  known object has an explicit marker and changes the hash.
- `match` and differences against the compiled reference. A successful command
  exit means the observation completed; inspect `match` for equality.
- Counts of unknown public objects, grouped by kind. A parent table name is
  included only when that table is already known.
- Counts for non-public metadata and other roles, plus two booleans indicating
  whether the migration owner owns the database and public schema. The owner
  symbol comes from `public.organizations`.
- With `-seed-counts`, counts and hashes of sorted, committed global seed keys.
  Version 0 reads only `permissions(resource, action)`. It never emits the keys
  or counts tenant/business rows.

Unknown objects do not change the known-object hash. A matching hash therefore
does not mean there are no extra objects: inspect `unknown` as well. Provider
roles and non-public schemas remain count-only. Other global relations have
documented structural exemptions, not permission to read their data.

`-mode json` includes the validated known-object snapshot. Definition and
expression text is represented by hashes; owners and grants use role symbols.
`-mode sha256` prints only the known-object hash.

`-list-unknown` is an explicit private owner-console option. It appends quoted
unknown public identities after the normal output, so that combined output is
not a JSON document. Keep it out of CI artifacts, issues and pull requests.
Only individually reviewed public identifiers may inform a future overlay;
no workflow asks an owner to publish the raw identifier list.

This observation does not authorize a schema change. A production overlay or
migration requires its separate review and release steps.

## Synthetic reference and tests

The reference lives in `internal/dbcatalog/golden/catalog.json`, with baseline
history and a reasoned global-relation allowlist beside it. Generate changes
only from the test-only empty bootstrap in a disposable loopback PostgreSQL
cluster. Never replace the reference to make an unexplained production
difference disappear.

The catalog tests are named `TestTenantRLS_Catalog*`. The required
`tenant-isolation` job runs them under `^TestTenantRLS_`; the general race job
excludes only `^TestTenantRLS_Catalog` to avoid duplicating the expensive
bootstrap. Each test binary creates one template database and clones it for
individual cases. Its caller-provided `TEST_DATABASE_URL` is only the local
administrative connection; it is not adopted or bootstrapped.

`TestCompatBootstrapExport` skips unless `COMPAT_BOOTSTRAP_DSN` is explicitly
set. Its caller must create an empty disposable loopback database, connect as
its non-superuser database/public-schema owner, and precreate the distinct
unprivileged runtime role named by `COMPAT_BOOTSTRAP_RUNTIME_ROLE` (default
`rereply_app`). The test refuses a nonempty public schema before writing,
bootstraps and verifies the future catalog, then leaves that database for the
compatibility consumer. The caller owns cleanup and the separate runtime
login/old-version/new-version proofs. No bootstrap code is included in the
release command.
