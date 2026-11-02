# Gmail relay command drain

The Gmail command owns its sync loop, forwarding loop, HTTP server, shared-store
closure and final report. A signal or any component return starts one absolute
30-second drain budget. An unexpected component return, including nil, remains a
runtime failure even when its peers drain successfully.

The owner requests worker cancellation and HTTP shutdown independently. It keeps
HTTP request contexts intact for graceful shutdown. A returned HTTP serve call
alone does not prove active handlers completed: both worker returns, the HTTP
serve return and successful HTTP Shutdown are required before either Redis store
can close. The two closes run independently within the same remaining budget.
Blocked signal cleanup, shutdown, a worker, either close or the reporter cannot
extend the owner wait.

Completed callbacks publish their outcomes through their own closed done channel.
Buffered wakeups are only notifications. Runtime cleanup no longer runs in
unconditional defers, and main does not synchronously log after the owner returns.
Startup failures still use startup cleanup before signal interception.

A complete drain means both workers and HTTP joined and both stores closed
successfully, including signal cleanup. Reporting has a separate completion bit.
An incomplete drain or report exits unsuccessfully; it does not claim all work
finished. At expiry the final report might not be emitted. The command does not
force-close stores under unfinished users, acknowledge work, reset leases,
advance cursors or invent resend/recovery outcomes.

The existing production forward worker retains its five-second,
cancellation-independent accepted-result settlement. The existing syncer retains
its heartbeat join, bounded cancellation-independent lease release, fenced cursor
commit and deterministic queue acceptance. Those packages are unchanged.

Prepared tests cover real HTTP handlers, every early component exit, blocked
shutdown callbacks/close/report, late completion, and a child-process exit check.
Two real Redis fixtures use unique test namespaces and local synthetic HTTP
providers: accepted forwarding completes before stores close and is not resent;
an interrupted sync leaves its old cursor until a fresh cycle deduplicates the
already accepted message and commits. The sync fixture uses the production
GmailClient and Syncer with a synthetic access-token provider; OAuth issuance is
outside this test. Real Redis fixtures explicitly skip without TEST_REDIS_URL;
hosted qualification must show both actually execute.

Validation requires source review, formatting, lint/build, hosted named
Go/race/Redis tests, generated closure checks and fresh combined release
qualification. The child-process fixture
runs the command-local owner in the Go test binary and follows main's direct-exit
pattern; it is not a deployed-process signal/grace measurement.

The Meta relay sibling, application background producer barrier, subscribers and
WebSocket joins, SLA current-tick cancellation, untracked acknowledged mutations,
and calling/recording ownership remain separate work. Actual platform termination
grace and provider outcomes are not established by this source slice.
