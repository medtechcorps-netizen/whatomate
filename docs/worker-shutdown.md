# Standalone worker shutdown

The worker command now uses the existing independent 25-second shutdown owner
also used by the API command. A signal or the first loop return starts one
absolute budget. Cancellation is requested independently; the message, channel
outbox, channel AI-reply and Threads credential loops each report an actual join.
Each worker's own consumer Close runs only after all four loops return. Shared
PostgreSQL and Redis clients remain available for settlement through the grace
period. A blocked loop, close, cancellation callback or reporter cannot extend
the deadline owner. Pending work is reported as incomplete.

Worker construction completes before signal interception and before any worker
loop starts. This keeps Redis group initialization or startup logging from
holding a captured termination signal ahead of the shutdown deadline owner.

`Worker.Run` now joins its `maintainHeartbeat` child before reporting that its
message loop finished. This is distinct from inbound continuation and Gmail sync
heartbeats, which already had local joins. Those paths are unchanged.

Channel discovery and claim queries now receive the loop's cancellation context
through an ephemeral Worker database view. Worker currently contains no mutex or
waitgroup; this view copies references and changes only its database session.
Provider attempts and accepted-result finalization still use the original Worker
and their existing contexts, leases and commit-ambiguity rules. No H1 ownership
scope, automatic resend policy, status repair, schema or permission seed changes
are included.

Prepared tests cover all four loop joins before Close, early failure/normal exit,
blocked work/close/reporting, normal child-process exit, heartbeat join ordering,
and actual PostgreSQL pool-wait cancellation for discovery and claim phases.
They require hosted Go/race/PostgreSQL execution; source review is not a pass.
The real deployment termination grace still needs verification. This is only the
standalone-worker portion of the broader lifecycle objective: app background
tasks, SLA, WebSockets/subscribers, calling, and relay command drains remain open.
