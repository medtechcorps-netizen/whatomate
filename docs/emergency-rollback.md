# Emergency rollback (owner-run, DigitalOcean console)

Use this when the Release pipeline cannot restore production for you:

- a Release run ended with exit 3 (MANUAL INTERVENTION);
- the pipeline or GitHub is unavailable, or the deploy token expired;
- production is broken right after a deploy and you cannot wait for a
  pipeline rollback.

Do not use it:

- for database problems: an app rollback never restores data;
- after an exit-2 run: that run already rolled production back and health
  passed.

## Before you start

1. Note the time.
2. Open the newest `prod-*` release record on GitHub (or the last run's
   summary) and copy its three digests: web, meta-relay, gmail-relay. These
   are the known-good images.
3. Cancel any Release run that is waiting for review or running.

## Steps

1. DigitalOcean > Apps > rereply > Activity.
2. Find the deployment whose web image digest matches the newest good record
   (the first 12 hex characters are enough). Only the ten most recent
   successful deployments are eligible for rollback.
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

1. Dispatch Release with mode `rollback` and `target_release` set to the
   record whose digests are now live. The production job sees that live
   already equals the target (the reconcile case), sends no PUT, re-checks
   health and writes the record.
2. If production runs something that matches no record, dispatch mode
   `rollback` with `target_release` set to the latest record (the restore
   case): one PUT back to the latest record's digests, then a restore record.
3. Open an issue describing what happened (no identifiers or hostnames).
4. If health still fails, contact DigitalOcean support.
