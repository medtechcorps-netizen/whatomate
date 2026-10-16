package handlers

// H1 liveness regressions from the adversarial liveness review of 953a2654.
//
// (1) Status-receipt lock inversion. The operator marker of a continuation send used
// to lock the independently committed response FOR UPDATE on the continuation
// transaction (tx A) until tx A committed. A WhatsApp status receipt for that response
// locks the contact FOR UPDATE and then waits for the message row; the continuation's
// next contact lock (the graph handover, or a later send) then waits for the receipt
// while tx A waits for it in Go. PostgreSQL never sees that cycle, and every later
// inbound message of the organization queued behind the handover's fence. The marker
// now commits in its own short transaction; these tests drive the real receipt into
// that exact window and require it to complete while tx A is still open.
//
// (2) Defence in depth: a nested handover's lock waits are bounded (lock_timeout) and
// a final 55P03 is a refunded wedge-guard deferral with nothing committed.
//
// Every case drives real ProcessMessage settlement on a dedicated pool (h1OpenPool);
// callbacks are registered only on that pool. None of these tests calls t.Parallel.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// h1PlainApp is a root receiver on the fixture's dedicated pool for webhook-shaped
// work (status receipts, later inbound admissions). The test database has no RLS
// organization resolver function, so it resolves the organization the non-RLS way;
// the receipt's lock order (WAMID, account, contact FOR UPDATE, message FOR UPDATE)
// is the production one.
func h1PlainApp(env *h1HandoverEnv) *App {
	return &App{
		DB:         env.pool,
		Log:        env.app.Log,
		Config:     &config.Config{},
		WhatsApp:   env.app.WhatsApp,
		HTTPClient: env.app.HTTPClient,
		Redis:      env.app.Redis,
	}
}

// h1PoolSessionReport lists this package's dedicated-pool backends with their blockers.
func h1PoolSessionReport(db *gorm.DB) string {
	type row struct {
		Pid      int
		Blockers string
		State    string
		Wait     string
		Query    string
	}
	var rows []row
	_ = db.Raw(`SELECT pid, COALESCE(array_to_string(pg_blocking_pids(pid), ','), '') AS blockers,
		COALESCE(state, '') AS state, COALESCE(wait_event_type, '-') || '/' || COALESCE(wait_event, '-') AS wait,
		LEFT(regexp_replace(query, '\s+', ' ', 'g'), 160) AS query
		FROM pg_stat_activity WHERE application_name = 'h1_pool_bound_test' ORDER BY pid`).Scan(&rows).Error
	out := &strings.Builder{}
	for _, r := range rows {
		fmt.Fprintf(out, "  pid=%d blockers=[%s] state=%s wait=%s q=%q\n", r.Pid, r.Blockers, r.State, r.Wait, r.Query)
	}
	return out.String()
}

// h1StatusReceipt delivers a real "delivered" receipt for wamid and waits at most
// limit for it to return. It never calls require: it may run on a non-test goroutine.
type h1StatusReceipt struct {
	returned bool
	err      error
	elapsed  time.Duration
	report   string
	pending  chan error
}

func h1DeliverStatusReceipt(env *h1HandoverEnv, receipts *App, wamid string, limit time.Duration) h1StatusReceipt {
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- receipts.processStatusUpdate(env.account.PhoneID, WebhookStatus{
			ID: wamid, Status: "delivered",
			Timestamp:   strconv.FormatInt(time.Now().Unix(), 10),
			RecipientID: env.work.Contact.PhoneNumber,
		})
	}()
	select {
	case err := <-done:
		return h1StatusReceipt{returned: true, err: err, elapsed: time.Since(started)}
	case <-time.After(limit):
		return h1StatusReceipt{report: h1PoolSessionReport(env.shared), pending: done}
	}
}

// h1RunWithWatchdog runs one real ProcessMessage. A run that is still blocked after
// limit is a permanent wedge: the test pool's backends are terminated so the test can
// finish, and the test fails with the blocking report.
func h1RunWithWatchdog(t *testing.T, env *h1HandoverEnv, now time.Time, limit time.Duration) (time.Duration, error) {
	t.Helper()
	started := time.Now()
	runDone := make(chan error, 1)
	go func() { runDone <- env.run(t, now) }()
	select {
	case err := <-runDone:
		return time.Since(started), err
	case <-time.After(limit):
		report := h1PoolSessionReport(env.shared)
		_ = env.shared.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'h1_pool_bound_test'`).Error
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
		}
		t.Fatalf("PERMANENT WEDGE: the continuation did not return within %s\n%s", limit, report)
		return 0, nil
	}
}

// h1RequireLiveAfterRun: nothing of the run is left holding a connection or a
// transaction, a later inbound message of another contact of the same organization
// is admitted (its organization fence is free), and /ready reports a healthy database.
func h1RequireLiveAfterRun(t *testing.T, env *h1HandoverEnv) {
	t.Helper()
	testutil.AssertEventually(t, func() bool { return env.sqlPool.Stats().InUse == 0 }, 5*time.Second,
		"no pooled connection is still in use after the run")
	var idleInTransaction int64
	require.NoError(t, env.shared.Raw(`SELECT count(*) FROM pg_stat_activity
		WHERE application_name = 'h1_pool_bound_test' AND state LIKE 'idle in transaction%'`).Scan(&idleInTransaction).Error)
	assert.Zero(t, idleInTransaction, "no transaction of the run may remain open")

	later := inboundContinuationTextMessage(t, "wamid.h1-live-admit-"+uuid.NewString(),
		"6064"+uuid.NewString()[:8], "A later patient")
	admitter := h1PlainApp(env)
	admitted := make(chan error, 1)
	go func() {
		_, _, err := admitter.persistIncomingMessageBeforeAck(env.account.PhoneID, later, "H1 later patient")
		admitted <- err
	}()
	select {
	case err := <-admitted:
		require.NoError(t, err, "a later inbound message of the organization is admitted")
	case <-time.After(10 * time.Second):
		t.Fatalf("a later inbound message of the organization was not admitted within 10s\n%s", h1PoolSessionReport(env.shared))
	}

	req := testutil.NewGETRequest(t)
	_ = env.app.ReadyCheck(req)
	assert.NotContains(t, string(req.RequestCtx.Response.Body()), "Database", "/ready must report a healthy database")
}

// The graph transfer node's body is durably sent; a status receipt for it arrives
// while the continuation transaction is still open and the handover has not started
// yet. The receipt must complete at once (tx A holds no lock on the body row), and the
// handover must then commit exactly once.
func TestH1GraphTransferStatusReceiptDuringHandover(t *testing.T) {
	for _, route := range []string{"queue", "team"} {
		t.Run(route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route:   route,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			receipts := h1PlainApp(env)
			var (
				setupErr error
				body     models.Message
				receipt  h1StatusReceipt
			)
			ran := env.h1AfterOuterAdvisoryCount(t, "h1:receipt_during_handover:"+route, func() {
				if err := env.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?",
					env.org.ID, env.work.Contact.ID, models.DirectionOutgoing).
					Order("created_at DESC").First(&body).Error; err != nil {
					setupErr = err
					return
				}
				receipt = h1DeliverStatusReceipt(env, receipts, body.WhatsAppMessageID, 10*time.Second)
			})

			elapsed, runErr := h1RunWithWatchdog(t, env, env.baseTime, 60*time.Second)
			if receipt.pending != nil {
				select {
				case receipt.err = <-receipt.pending:
				case <-time.After(10 * time.Second):
				}
			}
			require.True(t, ran.Load(), "the barrier must run after the body and before the handover")
			require.NoError(t, setupErr)
			require.NotEmpty(t, body.WhatsAppMessageID, "the body must be sent before the handover")
			require.True(t, receipt.returned,
				"a status receipt for the transfer body must not wait for the continuation transaction\n%s", receipt.report)
			require.NoError(t, receipt.err)
			assert.Less(t, receipt.elapsed, 5*time.Second)
			require.NoError(t, runErr)
			assert.Less(t, elapsed, 30*time.Second)

			state := env.state(t)
			assert.EqualValues(t, 1, state.ActiveTransfers)
			assert.Len(t, env.transfers(t), 1)
			assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
			assert.True(t, state.InboundCompleted)
			assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
			assert.Equal(t, 1, state.T1Entries)
			assert.EqualValues(t, 1, state.ProviderCalls, "the body is sent once")
			require.Len(t, state.Outgoing, 1)
			assert.Equal(t, models.MessageStatusDelivered, state.Outgoing[0].Status, "the receipt was applied")
			assert.NotEmpty(t, state.Outgoing[0].Metadata["inbound_continuation_action_key"],
				"the operator marker still commits, independently")
			assert.EqualValues(t, 1, state.Ledger)
			assert.True(t, state.ContactCutoff)
			h1RequireLiveAfterRun(t, env)
		})
	}
}

// Two message nodes in one graph run: a status receipt for the FIRST reply arrives
// before the second reply's send locks the contact. The receipt must complete at once
// and the second reply must then be sent.
func TestH1GraphStatusReceiptBetweenTwoSends(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{})
	flow := &models.ChatbotFlow{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
		WhatsAppAccount: env.account.Name, Name: "h1-two-messages", IsEnabled: true,
		Graph: models.JSONB{"version": 2, "entry_node": "m1", "nodes": []any{
			map[string]any{"id": "m1", "type": "message", "label": "one", "config": map[string]any{"message": "First reply"}},
			map[string]any{"id": "m2", "type": "message", "label": "two", "config": map[string]any{"message": "Second reply"}},
			map[string]any{"id": "e1", "type": "end", "label": "done", "config": map[string]any{}},
		}, "edges": []any{
			map[string]any{"from": "m1", "to": "m2", "condition": "default"},
			map[string]any{"from": "m2", "to": "e1", "condition": "default"},
		}},
	}
	require.NoError(t, env.shared.Create(flow).Error)
	env.flow = flow
	receipts := h1PlainApp(env)

	var (
		once     sync.Once
		fired    atomic.Bool
		setupErr error
		first    models.Message
		receipt  h1StatusReceipt
	)
	// The second reply's AI-attempt guard (its organization KEY SHARE), taken after the
	// first reply was sent and before the second reply's send locks the contact.
	const name = "h1:receipt_between_sends"
	require.NoError(t, env.pool.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if !env.graphRunning.Load() || env.calls.Load() != 1 ||
			!strings.Contains(tx.Statement.SQL.String(), "FOR KEY SHARE") {
			return
		}
		once.Do(func() {
			fired.Store(true)
			if err := env.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?",
				env.org.ID, env.work.Contact.ID, models.DirectionOutgoing).
				Order("created_at").First(&first).Error; err != nil {
				setupErr = err
				return
			}
			receipt = h1DeliverStatusReceipt(env, receipts, first.WhatsAppMessageID, 10*time.Second)
		})
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Row().Remove(name) })

	elapsed, runErr := h1RunWithWatchdog(t, env, env.baseTime, 60*time.Second)
	if receipt.pending != nil {
		select {
		case receipt.err = <-receipt.pending:
		case <-time.After(10 * time.Second):
		}
	}
	require.True(t, fired.Load(), "the barrier must run between the two sends")
	require.NoError(t, setupErr)
	require.True(t, receipt.returned,
		"a status receipt for the first reply must not wait for the continuation transaction\n%s", receipt.report)
	require.NoError(t, receipt.err)
	assert.Less(t, receipt.elapsed, 5*time.Second)
	require.NoError(t, runErr)
	assert.Less(t, elapsed, 30*time.Second)

	state := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
	assert.True(t, state.InboundCompleted)
	assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
	assert.EqualValues(t, 2, state.ProviderCalls, "each reply is sent once")
	require.Len(t, state.Outgoing, 2)
	assert.EqualValues(t, 2, state.Ledger)
	var delivered models.Message
	require.NoError(t, env.shared.Where("id = ?", first.ID).First(&delivered).Error)
	assert.Equal(t, models.MessageStatusDelivered, delivered.Status, "the receipt was applied")
	h1RequireLiveAfterRun(t, env)
}

// The send marker never runs on the continuation's own transaction: it commits
// independently (visible before tx A ends), tx A keeps no lock on the response row,
// and inside the AI-attempt fence its connection wait is the 2s best-effort budget.
func TestH1SendMarkerCommitsOutsideContinuationTransaction(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, shared, organization.ID, testutil.WithContactAccount(account.Name))
	response := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		WhatsAppAccount: account.Name, ContactID: contact.ID,
		WhatsAppMessageID: "wamid.h1-marker-" + uuid.NewString(), Direction: models.DirectionOutgoing,
		MessageType: models.MessageTypeText, Content: "durable reply", Status: models.MessageStatusSent,
		Metadata: models.JSONB{},
	}
	require.NoError(t, shared.Create(&response).Error)

	pool, sqlDB := h1OpenPool(t, 3)
	root := h1RootApp(pool, true)
	txA := pool.Begin()
	require.NoError(t, txA.Error)
	t.Cleanup(func() { txA.Rollback() })
	nested := h1NestedApp(root, organization.ID)
	nested.DB = txA // the continuation transaction
	nested.inboundContinuation.ContactID = contact.ID
	nested.inboundContinuation.MessageID = uuid.New()
	nested.inboundContinuation.WAMID = "wamid.h1-marker-inbound"
	nested.inboundContinuation.attemptGuarded = true

	require.NoError(t, nested.markInboundContinuationAction(response.ID, "inbound-action:h1-marker"))
	// Committed and unlocked while tx A is still open.
	var stored models.Message
	require.NoError(t, shared.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("SELECT * FROM messages WHERE id = ? FOR UPDATE NOWAIT", response.ID).Scan(&stored).Error
	}), "the continuation transaction must hold no lock on the response row")
	assert.Equal(t, "inbound-action:h1-marker", stored.Metadata["inbound_continuation_action_key"])
	assert.Equal(t, "wamid.h1-marker-inbound", stored.Metadata["inbound_continuation_wamid"])

	// Pool exhausted (tx A plus two held): the best-effort marker yields after ~2s.
	release := h1HoldConnections(t, sqlDB, 2)
	started := time.Now()
	err := nested.markInboundContinuationAction(response.ID, "inbound-action:h1-marker-2")
	elapsed := time.Since(started)
	release()
	h1RequireWedgeGuard(t, err)
	assert.GreaterOrEqual(t, elapsed, 1900*time.Millisecond)
	assert.Less(t, elapsed, 5*time.Second, "the marker must not keep the AI-attempt fence for the 45s in-fence budget")
}

// h1FailTransferInsertsWithLockTimeout makes the next `times` agent_transfers inserts
// fail exactly as an expired lock_timeout does (SQLSTATE 55P03), and records the
// lock_timeout in effect on the handover's own transaction.
func h1FailTransferInsertsWithLockTimeout(t *testing.T, db *gorm.DB, name string, times int32) (*atomic.Int32, *atomic.Value) {
	t.Helper()
	var failed atomic.Int32
	var observed atomic.Value
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if !h1IsAgentTransferCreate(tx) {
			return
		}
		var setting string
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SHOW lock_timeout").Row().Scan(&setting); err == nil {
			observed.Store(setting)
		}
		if failed.Load() < times {
			failed.Add(1)
			_ = tx.AddError(&pgconn.PgError{Code: "55P03", Severity: "ERROR", Message: "canceling statement due to lock timeout"})
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(name) })
	return &failed, &observed
}

func h1RequireLockTimeoutDeferral(t *testing.T, err error) {
	t.Helper()
	h1RequireWedgeGuard(t, err)
	assert.Equal(t, "55P03", postgresErrorCode(err), "the lock-timeout cause stays inspectable")
	assert.False(t, inboundContinuationRequiresManualReview(err))
	assert.False(t, inboundContinuationStoppedByPolicy(err))
}

// A nested handover runs with a bounded lock wait, and when every canonical-write
// attempt ends in 55P03 the continuation is a refunded wedge-guard deferral with
// nothing committed; the replay hands over exactly once without resending the body.
func TestH1HandoverLockTimeoutIsRefundedDeferral(t *testing.T) {
	for _, route := range []string{"queue", "team"} {
		t.Run("graph_"+route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route:   route,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			failed, observed := h1FailTransferInsertsWithLockTimeout(t, env.pool, "h1:handover_lock_timeout:"+route,
				canonicalContactWriteAttempts)

			firstErr := env.run(t, env.baseTime)
			h1RequireLockTimeoutDeferral(t, firstErr)
			assert.EqualValues(t, canonicalContactWriteAttempts, failed.Load(), "every canonical-write attempt retried the 55P03")
			assert.Equal(t, "3s", observed.Load(), "the nested handover bounds its lock waits")
			first := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
			assert.Equal(t, 0, first.Job.Attempts, "a lock-timeout handover is a refunded deferral")
			assert.Equal(t, 1, h1JobDeferrals(first.Job))
			assert.False(t, first.InboundCompleted)
			assert.Zero(t, first.ActiveTransfers)
			assert.Equal(t, models.SessionStatusActive, first.Session.Status)
			assert.Nil(t, first.Session.CompletedAt)
			assert.False(t, first.ContactCutoff)
			assert.EqualValues(t, 1, first.ProviderCalls)
			assert.Len(t, first.Outgoing, 1)

			require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
			replayed := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
			assert.True(t, replayed.InboundCompleted)
			assert.EqualValues(t, 1, replayed.ActiveTransfers)
			assert.Len(t, env.transfers(t), 1)
			assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
			assert.Equal(t, 1, replayed.T1Entries)
			assert.EqualValues(t, 1, replayed.ProviderCalls, "recovery must not resend the body")
			assert.Len(t, replayed.Outgoing, 1)

			// SET LOCAL ended with the handover transaction.
			var setting string
			require.NoError(t, env.pool.Raw("SHOW lock_timeout").Row().Scan(&setting))
			assert.Equal(t, "0", setting)
		})
	}

	t.Run("chatbot_disabled", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, models.BusinessHoursConfig{})
		failed, observed := h1FailTransferInsertsWithLockTimeout(t, app.DB, "h1:disabled_lock_timeout",
			canonicalContactWriteAttempts)
		baseTime := time.Now().UTC()
		processor := NewInboundContinuationProcessor(app, time.Second)
		processor.now = func() time.Time { return baseTime }

		err := processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
		require.ErrorContains(t, err, "create chatbot-disabled queue transfer")
		h1RequireLockTimeoutDeferral(t, err)
		assert.EqualValues(t, canonicalContactWriteAttempts, failed.Load())
		assert.Equal(t, "3s", observed.Load())
		job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
		assert.Equal(t, 0, job.Attempts)
		assert.Equal(t, 1, h1JobDeferrals(job))
		assert.Zero(t, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))

		processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
		require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
		job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
		assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))
	})
}

// h1LockTimeoutLogProbe releases the outside lock once the real handover failure is
// logged (or on its watchdog), so nothing else of the run can queue behind it.
type h1LockTimeoutLogProbe struct {
	mu       sync.Mutex
	matched  string
	release  chan struct{}
	released sync.Once
}

func (p *h1LockTimeoutLogProbe) Write(data []byte) (int, error) {
	line := string(data)
	if strings.Contains(line, "Failed to create transfer to queue") && strings.Contains(line, "lock timeout") {
		p.mu.Lock()
		if p.matched == "" {
			p.matched = strings.TrimSpace(line)
		}
		p.mu.Unlock()
		p.released.Do(func() { close(p.release) })
	}
	return len(data), nil
}

// The real bound: an outside session holds the contact row while the graph handover
// asks for it (after the body, on the pre-handover boundary). Without the bound this
// handover would wait as long as the holder, pinning the organization fence; with it
// every canonical-write attempt expires (55P03) after a few seconds, nothing commits,
// and the replay hands over once.
func TestH1GraphTransferHandoverLockWaitIsBounded(t *testing.T) {
	h1RequireSlow(t)
	env := h1HandoverFixture(t, h1HandoverOptions{flowCfg: map[string]any{"body": "Connecting you to a human."}})
	probe := &h1LockTimeoutLogProbe{release: make(chan struct{})}
	env.app.Log = logf.New(logf.Opts{Writer: probe, Level: logf.ErrorLevel})

	holderDone := make(chan struct{})
	var holderErr error
	var heldFor time.Duration
	ran := env.h1AfterOuterAdvisoryCount(t, "h1:outside_contact_lock", func() {
		ready := make(chan error, 1)
		go func() {
			defer close(holderDone)
			holder := env.shared.Begin()
			if holder.Error != nil {
				ready <- holder.Error
				return
			}
			defer holder.Rollback()
			var id uuid.UUID
			if err := holder.Raw("SELECT id FROM contacts WHERE id = ? FOR UPDATE", env.work.Contact.ID).Row().Scan(&id); err != nil {
				ready <- err
				return
			}
			ready <- nil
			lockedAt := time.Now()
			select {
			case <-probe.release:
			case <-time.After(60 * time.Second): // watchdog: the handover never gave up
			}
			heldFor = time.Since(lockedAt)
		}()
		holderErr = <-ready // the handover starts only once the row is held
	})

	elapsed, firstErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
	require.True(t, ran.Load())
	select {
	case <-holderDone:
	case <-time.After(70 * time.Second):
		t.Fatal("the outside contact lock was never released")
	}
	require.NoError(t, holderErr, "the outside session must hold the contact row")
	probe.mu.Lock()
	matched := probe.matched
	probe.mu.Unlock()
	require.NotEmpty(t, matched, "the real handover must fail on its bounded lock wait (held %s)", heldFor)
	t.Logf("H1_HANDOVER_LOCK_WAIT elapsed=%s held=%s log=%s", elapsed, heldFor, matched)

	h1RequireLockTimeoutDeferral(t, firstErr)
	assert.GreaterOrEqual(t, heldFor, 15*time.Second, "six attempts of a 3s lock wait")
	assert.Less(t, heldFor, 30*time.Second, "all attempts stay within about 20s")
	first := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
	assert.Equal(t, 0, first.Job.Attempts)
	assert.Equal(t, 1, h1JobDeferrals(first.Job))
	assert.False(t, first.InboundCompleted)
	assert.Zero(t, first.ActiveTransfers)
	assert.Equal(t, models.SessionStatusActive, first.Session.Status)
	assert.False(t, first.ContactCutoff)
	assert.EqualValues(t, 1, first.ProviderCalls)

	require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
	replayed := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
	assert.True(t, replayed.InboundCompleted)
	assert.EqualValues(t, 1, replayed.ActiveTransfers)
	assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
	assert.EqualValues(t, 1, replayed.ProviderCalls, "recovery must not resend the body")
	assert.Len(t, replayed.Outgoing, 1)
	h1RequireLiveAfterRun(t, env)
}
