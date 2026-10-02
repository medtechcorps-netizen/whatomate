# Emergency rollback (owner-run, DigitalOcean console)

Use this when the Release pipeline cannot restore production for you:

- a Release run ended with exit 3 (MANUAL INTERVENTION);
- the `Release production` job ended `cancelled` or `timed out` (treat it as
  exit 3: it may have stopped between the PUT and the end of its rollback);
- the pipeline or GitHub is unavailable, or the deploy token expired;
- production is broken right after a deploy and you cannot wait for a
  pipeline rollback.

Do not use it:

- for database problems: an app rollback never restores data;
- after an exit-2 run: that run already rolled production back and health
  passed.

## Before you start

1. Note the time.
2. Pick the known-good record: the newest record that was running healthily
   BEFORE the release that broke production. It is not always the newest
   record:
   - If the bad release wrote a record (it passed the six health probes and
     broke later, for example a WhatsApp or inbox regression the probes
     cannot see), the newest record IS the bad release, and it is also what
     is running. Use its `previous` record instead: "Previous record" in the
     release notes, or `previous` in its `release-manifest.json` asset. If
     that record is itself suspect, follow `previous` back again.
   - If the failing run wrote no record (exit 3, cancelled or timed out):
     after a `promote` run, the newest record is the known-good one; after a
     `rollback`-mode run, the newest record is the release it was rolling
     back from, so use the run's `target_release` if that is the state you
     want (or the newest record, if that was healthy).
   - If `previous` is `prod-0000`, release #0's digests are in
     `release/deployment/ship-bootstrap-record.json`.
3. Copy that record's three digests: web, meta-relay, gmail-relay.
4. Cancel any Release run that is waiting for review or running.

## Steps

1. DigitalOcean > Apps > rereply > Activity.
2. Find the deployment whose web image digest matches the known-good record
   you picked above (the first 12 hex characters are enough). Only the ten
   most recent successful deployments are eligible for rollback.
3. Choose Rollback on that deployment. Database data is not rolled back.
4. Wait until the deployment is Active. The PRE_DEPLOY `rls-migrate` job runs
   verify-only with the old web image.
5. Verify health in a browser or terminal against the app's URL:
   - `/health` and `/ready` return 200;
   - `/meta-relay/livez`, `/meta-relay/readyz`, `/gmail-relay/livez` and
     `/gmail-relay/readyz` return 204;
   - the runtime logs show no webhook 503s.
6. Commit the rollback. DigitalOcean keeps the app pinned to a rollback until
   it is committed or reverted, and the pipeline refuses while the app is
   pinned.

The exact console labels may differ slightly; confirm them on first use.
Confirm on first use too that a console rollback re-applies the chosen
deployment's whole app spec, including environment variable and secret
values from that time. Any environment or secret rotation made since then
must be redone through the reviewed path afterwards; the pipeline compares
image digests only and will not notice.

## What not to do

- No console edits of environment variables, the app spec, components,
  domains, the VPC, the image source or tag, and no auto-deploy.
- Do not Revert a rollback you mean to keep.
- Do not roll back to a deployment that matches no record.
- Never run `rls-migrate -rollback` or manual SQL.
- No database restore as part of an app rollback; that is a separate owner
  incident decision.
- Do not delete the app, the databases or the GHCR images.
- Do not re-run failed Release runs.
- Claude never approves production.

## What follows: reconcile the record

1. If the record now live is the latest record (for example you rolled back
   after an exit-3 promote that wrote no record), nothing needs reconciling.
   A dispatch would only refuse with `nothing-to-roll-back`.
2. If the record now live is an older record, dispatch Release with mode
   `rollback` and `target_release` set to that record. The production job
   sees that live already equals the target (the reconcile case), sends no
   PUT, re-checks health and writes the record.
3. Only if production runs something that matches no record, dispatch mode
   `rollback` with `target_release` set to the latest record (the restore
   case): one PUT back to the latest record's digests, then a restore record.
   The run refuses a restore while live equals an older record
   (`drift:live-matches-record`); reconcile with that record instead.
4. Open an issue describing what happened (no identifiers or hostnames).
5. If health still fails, contact DigitalOcean support.
