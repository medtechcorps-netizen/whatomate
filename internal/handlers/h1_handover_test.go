package handlers

// H1 handover durability regressions. On H1 head 02e6a724 a graph transfer node
// committed its session completion in its own transaction BEFORE the handover; the
// handover's bounded 15s pool wait could then expire, its error was swallowed, and
// the job, the inbound obligation and every replay completed with zero transfers
// (reproduced by Codex, reports c80c56a1 and e903dd5d). The graph's terminal
// checkpoint now commits atomically with the handover inside saveAndFinalizeTransfer,
// and the handover error reaches p.fail.
//
// Every case drives real ProcessMessage settlement (claim, phase two's outer tenant
// transaction, p.fail, replay); only dispatch is fixture-selected unless a test says
// otherwise. Failure-injection callbacks are registered only on dedicated pools
// (h1OpenPool), never on the shared test *gorm.DB. None of these tests calls
// t.Parallel.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/contacthandoff"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

type h1HandoverOptions struct {
	route    string // "queue" (default) or "team"
	flowCfg  map[string]any
	settings func(*models.ChatbotSettings)
	// hub registers one realtime client and counts committed agent_transfer envelopes.
	hub bool
	// foreignAccount dispatches the graph with an account of another organization.
	foreignAccount bool
}

// h1HandoverEnv is one persisted inbound message, its active chatbot session and a
// single-node transfer flow, executed by a real InboundContinuationProcessor whose
// dispatch runs the graph on phase two's outer transaction.
type h1HandoverEnv struct {
	app        *App
	shared     *gorm.DB // fixture/readback pool, same database
	org        *models.Organization
	account    *models.WhatsAppAccount
	foreignOrg *models.Organization
	work       *persistedIncomingMessage
	session    *models.ChatbotSession
	flow       *models.ChatbotFlow
	team       *models.Team
	pool       *gorm.DB // the App's private root pool
	sqlPool    *sql.DB
	calls      *atomic.Int32
	envelopes  *atomic.Int32 // committed TypeAgentTransfer envelopes (hub only)
	processor  *InboundContinuationProcessor
	baseTime   time.Time

	dispatchAccount *models.WhatsAppAccount
	graphCalls      atomic.Int32
	graphRunning    atomic.Bool
	graphReturned   atomic.Bool
	execution       atomic.Pointer[App]
	current         atomic.Pointer[models.ChatbotSession]

	mu             sync.Mutex
	graphErrors    []string
	loadedActivity []time.Time
}

func h1HandoverFixture(t *testing.T, opts h1HandoverOptions) *h1HandoverEnv {
	t.Helper()
	app := newProcessorTestApp(t)
	env := &h1HandoverEnv{app: app, shared: app.DB}
	env.org, env.account = createProcessorTestOrg(t, app)
	settings := &models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     env.org.ID,
		WhatsAppAccount:    env.account.Name,
		IsEnabled:          true,
		SessionTimeoutMins: 30,
		SLA:                models.SLAConfig{Enabled: true, ResponseMinutes: 15},
		AI:                 models.AIConfig{Enabled: false},
	}
	if opts.settings != nil {
		opts.settings(settings)
	}
	require.NoError(t, env.shared.Create(settings).Error)
	message := inboundContinuationTextMessage(t, "wamid.h1-handover-"+uuid.NewString(),
		"6062"+uuid.NewString()[:8], "Please connect me to a human")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(env.account.PhoneID, message, "H1 handover patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	env.work = work
	env.session = &models.ChatbotSession{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
		ContactID: work.Contact.ID, WhatsAppAccount: env.account.Name,
		PhoneNumber: work.Contact.PhoneNumber, Status: models.SessionStatusActive,
		SessionData: models.JSONB{}, StartedAt: time.Now(), LastActivityAt: time.Now(),
	}
	require.NoError(t, env.shared.Create(env.session).Error)
	flowConfig := map[string]any{}
	for key, value := range opts.flowCfg {
		flowConfig[key] = value
	}
	if opts.route == "team" {
		env.team = &models.Team{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
			Name: "H1 handover " + uuid.NewString(), IsActive: true,
			AssignmentStrategy: models.AssignmentStrategyRoundRobin,
		}
		require.NoError(t, env.shared.Create(env.team).Error)
		flowConfig["team_id"] = env.team.ID.String()
	}
	env.flow = newTransferFlow(t, app, env.org, env.account, flowConfig)
	require.Empty(t, env.flow.OnCompleteAction, "no completion webhook may run")
	if opts.foreignAccount {
		var foreignAccount *models.WhatsAppAccount
		env.foreignOrg, foreignAccount = createProcessorTestOrg(t, app)
		env.dispatchAccount = foreignAccount
	}

	env.pool, env.sqlPool = h1OpenPool(t, 10)
	app.DB = env.pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	env.calls = h1CountingProvider(t, app, http.StatusOK)
	if opts.hub {
		env.envelopes = h1CountTransferEnvelopes(t, app, env.org.ID)
	}
	env.baseTime = time.Now().UTC()
	// The DEFAULT lease (5 min) keeps the heartbeat at its 30s cap.
	env.processor = NewInboundContinuationProcessor(app, time.Second)
	env.processor.now = func() time.Time { return env.baseTime }
	env.processor.process = env.dispatch
	return env
}

// dispatch is the only fixture-selected step: the real processor still claims the
// durable job, opens phase two, settles and replays it.
func (env *h1HandoverEnv) dispatch(_ context.Context, execution *App, loaded *persistedIncomingMessage) error {
	env.graphCalls.Add(1)
	var current models.ChatbotSession
	if err := execution.DB.Where("id = ? AND organization_id = ?", env.session.ID, env.org.ID).First(&current).Error; err != nil {
		return err
	}
	account := &loaded.Account
	if env.dispatchAccount != nil {
		account = env.dispatchAccount
	}
	env.mu.Lock()
	env.loadedActivity = append(env.loadedActivity, current.LastActivityAt)
	env.mu.Unlock()
	env.current.Store(&current)
	env.execution.Store(execution)
	env.graphReturned.Store(false)
	env.graphRunning.Store(true)
	err := execution.runChatGraph(account, &loaded.Contact, &current, env.flow, "help", "", nil)
	env.graphRunning.Store(false)
	env.graphReturned.Store(true)
	env.mu.Lock()
	env.graphErrors = append(env.graphErrors, fmt.Sprint(err))
	env.mu.Unlock()
	return err
}

func (env *h1HandoverEnv) run(t *testing.T, now time.Time) error {
	t.Helper()
	env.processor.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return env.processor.ProcessMessage(ctx, env.org.ID, env.work.Persisted.ID)
}

func (env *h1HandoverEnv) loaded(t *testing.T, index int) time.Time {
	t.Helper()
	env.mu.Lock()
	defer env.mu.Unlock()
	require.Greater(t, len(env.loadedActivity), index)
	return env.loadedActivity[index]
}

type h1HandoverState struct {
	Session           models.ChatbotSession
	Ordinal           int
	T1Entries         int
	Job               models.ScheduledJob
	InboundCompleted  bool
	InboundSuppressed bool
	ContactCutoff     bool
	ContactAssignee   *uuid.UUID
	ActiveTransfers   int64
	ProviderCalls     int32
	Outgoing          []models.Message
	Ledger            int64
	GraphCalls        int32
}

func (env *h1HandoverEnv) state(t *testing.T) h1HandoverState {
	t.Helper()
	var state h1HandoverState
	require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.session.ID, env.org.ID).First(&state.Session).Error)
	ordinal, err := chatGraphVisitOrdinal(&state.Session)
	require.NoError(t, err)
	state.Ordinal = ordinal
	state.T1Entries = h1PathEntries(&state.Session, "t1")
	state.Job = loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	var inbound models.Message
	require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.work.Persisted.ID, env.org.ID).First(&inbound).Error)
	state.InboundCompleted = inbound.Metadata["inbound_continuation_completed"] == true
	state.InboundSuppressed = inbound.Metadata[incomingAutomaticAISuppressedKey] == true
	var contact models.Contact
	require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.work.Contact.ID, env.org.ID).First(&contact).Error)
	_, state.ContactCutoff = contact.Metadata[contacthandoff.CutoffKey]
	state.ContactAssignee = contact.AssignedUserID
	state.ActiveTransfers = h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID)
	state.ProviderCalls = env.calls.Load()
	state.Outgoing = h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID)
	state.Ledger = h1ActionLedgerRows(t, env.shared, env.org.ID, env.work.Persisted.ID)
	state.GraphCalls = env.graphCalls.Load()
	return state
}

func (env *h1HandoverEnv) transfers(t *testing.T) []models.AgentTransfer {
	t.Helper()
	var transfers []models.AgentTransfer
	require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ?", env.org.ID, env.work.Contact.ID).
		Order("created_at").Find(&transfers).Error)
	return transfers
}

func h1PathEntries(session *models.ChatbotSession, node string) int {
	path, _ := session.SessionData["__path__"].([]any)
	count := 0
	for _, raw := range path {
		if entry, ok := raw.(map[string]any); ok && entry["node"] == node {
			count++
		}
	}
	return count
}

// h1CountTransferEnvelopes registers one realtime client for the organization and
// counts the agent_transfer (created) envelopes it receives.
func h1CountTransferEnvelopes(t *testing.T, app *App, organizationID uuid.UUID) *atomic.Int32 {
	t.Helper()
	hub := appwebsocket.NewHub(app.Log)
	go hub.Run()
	app.WSHub = hub
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organizationID)
	hub.Register(client)
	testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second,
		"handover realtime client registered")
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		for {
			select {
			case data := <-client.SendChan():
				for _, line := range bytes.Split(data, []byte("\n")) {
					var envelope struct {
						Type string `json:"type"`
					}
					if json.Unmarshal(line, &envelope) == nil && envelope.Type == appwebsocket.TypeAgentTransfer {
						count.Add(1)
					}
				}
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() { close(done) })
	return &count
}

// h1RequireEnvelopes waits for want envelopes, then requires that no more arrive.
func h1RequireEnvelopes(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()
	if want > 0 {
		testutil.AssertEventually(t, func() bool { return count.Load() >= want }, 2*time.Second,
			"committed agent_transfer envelope delivered")
	}
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, want, count.Load(), "agent_transfer (created) envelopes")
}

func h1IsSessionCompletionUpdate(tx *gorm.DB) bool {
	if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "chatbot_sessions" {
		return false
	}
	updates, ok := tx.Statement.Dest.(map[string]any)
	return ok && updates["status"] == models.SessionStatusCompleted
}

func h1IsCutoffContactRead(tx *gorm.DB) bool {
	if _, ok := tx.Statement.Dest.(*models.Contact); !ok {
		return false
	}
	return len(tx.Statement.Selects) == 2 && tx.Statement.Selects[0] == "id" && tx.Statement.Selects[1] == "metadata"
}

// h1IsOuterAdvisoryCount matches the graph handover's advisory agent_transfers Count
// on phase two's outer transaction.
func (env *h1HandoverEnv) h1IsOuterAdvisoryCount(tx *gorm.DB) bool {
	if !env.graphRunning.Load() || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "agent_transfers" {
		return false
	}
	execution := env.execution.Load()
	if execution == nil || tx.Statement.ConnPool != execution.DB.Statement.ConnPool {
		return false
	}
	_, isCount := tx.Statement.Dest.(*int64)
	return isCount
}

// h1HandoverWaitProbe observes the actual production error. It never injects an
// error into GORM or the application. Releasing here lets normal graph/inbound
// settlement proceed; waiting for ProcessMessage to return could itself block
// settlement behind the connection held by the test. (Ported from Codex's
// isolated diagnostic, reports c80c56a1 and e903dd5d.)
type h1HandoverWaitProbe struct {
	mu              sync.Mutex
	logs            bytes.Buffer
	pool            *sql.DB
	held            *sql.Conn
	timer           *time.Timer
	releaseOnce     sync.Once
	armed           bool
	blockedAt       time.Time
	timeoutAt       time.Time
	timeoutObserved bool
	releaseReason   string
	releaseError    string
	matchingLog     string
}

func (p *h1HandoverWaitProbe) Write(data []byte) (int, error) {
	p.mu.Lock()
	_, _ = p.logs.Write(data)
	line := string(data)
	transferFailure := strings.Contains(line, "Failed to create transfer to queue") ||
		strings.Contains(line, "Failed to create team transfer")
	matched := p.armed && transferFailure &&
		strings.Contains(line, "wedge guard: no pooled database connection within 15s") &&
		strings.Contains(line, "context deadline exceeded")
	if matched && !p.timeoutObserved {
		p.timeoutObserved = true
		p.timeoutAt = time.Now()
		p.matchingLog = strings.TrimSpace(line)
	}
	p.mu.Unlock()
	if matched {
		p.release("actual-transfer-timeout-log")
	}
	return len(data), nil
}

func (p *h1HandoverWaitProbe) release(reason string) {
	p.releaseOnce.Do(func() {
		p.mu.Lock()
		p.releaseReason = reason
		conn, timer := p.held, p.timer
		p.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		if conn != nil {
			if err := conn.Close(); err != nil {
				p.mu.Lock()
				p.releaseError = err.Error()
				p.mu.Unlock()
			}
		}
		// Restore this test's own root pool only; the shared pool is never touched.
		p.pool.SetMaxOpenConns(10)
		p.pool.SetMaxIdleConns(10)
	})
}

func (p *h1HandoverWaitProbe) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Duration(0)
	if !p.timeoutAt.IsZero() {
		elapsed = p.timeoutAt.Sub(p.blockedAt)
	}
	return map[string]any{
		"armed": p.armed, "actual_timeout_observed": p.timeoutObserved,
		"blocked_to_timeout_ms": elapsed.Milliseconds(),
		"release_reason":        p.releaseReason, "release_error": p.releaseError,
		"matching_production_log": p.matchingLog,
	}
}

// The permanent regression for the lost handover, at the corrected PRE-handover
// acquisition boundary: the body has durably succeeded, the session is still active
// and owned, and the exact root pool is saturated (Open2/InUse2/Idle0/Max2) when the
// graph handover asks it for the independent handover connection. The REAL 15s
// bounded wait must expire, nothing may be committed, the job must stay pending as a
// refunded deferral, and the replay must create exactly one handover without a
// second body. Codex's original barrier (after a committed completion) can never arm
// on this code: the session is no longer completed before the handover.
func TestH1GraphTransferPoolTimeoutPreservesHandover(t *testing.T) {
	h1RequireSlow(t)
	for _, route := range []string{"queue", "team"} {
		t.Run(route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route:   route,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			probe := &h1HandoverWaitProbe{pool: env.sqlPool}
			env.app.Log = logf.New(logf.Opts{Writer: probe, Level: logf.ErrorLevel})
			t.Cleanup(func() { probe.release("test-cleanup") })

			var barrierUsed atomic.Bool
			var barrierQueries atomic.Int32
			var preHandoverBarrierArmed bool
			var poolInUseAtBarrier int
			var poolStatsBeforeCap, poolStatsAfterCap, poolStatsAfterHeld sql.DBStats
			var rootPoolIdentity, callbackOuterIdentity bool
			var claimStart time.Time
			var claimToBarrier time.Duration
			var observedQueries []map[string]any
			callbackName := "h1:transfer_timeout_barrier:" + route
			require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if !env.graphRunning.Load() || barrierUsed.Load() || tx.Statement.Schema == nil ||
					tx.Statement.Schema.Table != "agent_transfers" {
					return
				}
				executionAtBarrier := env.execution.Load()
				require.NotNil(t, executionAtBarrier)
				outer := tx.Statement.ConnPool == executionAtBarrier.DB.Statement.ConnPool
				guarded := executionAtBarrier.inboundContinuation != nil && executionAtBarrier.inboundContinuation.attemptGuarded
				_, isCount := tx.Statement.Dest.(*int64)
				// Every agent_transfers query of the graph run is recorded: the body's
				// AI-fence policy reads hit agent_transfers on guard connections, so the
				// arming identity below (outer transaction, body done) is a filter.
				observedQueries = append(observedQueries, map[string]any{
					"outer_transaction": outer, "attempt_guarded": guarded,
					"provider_calls": env.calls.Load(), "count_query": isCount,
				})
				if !outer || env.calls.Load() != 1 {
					return
				}
				require.True(t, isCount, "the first outer agent_transfers query after the body must be the graph handover's advisory Count")
				claimToBarrier = time.Since(claimStart)
				// A fresh read through the independent fixture pool proves what is
				// COMMITTED: still the pre-run checkpoint (runChatGraph sets t1 only in
				// memory), active and owned.
				var committed models.ChatbotSession
				require.NoError(t, env.shared.Where("id = ? AND organization_id = ?", env.session.ID, env.org.ID).First(&committed).Error)
				require.Equal(t, models.SessionStatusActive, committed.Status, "the session must not complete before the handover")
				require.Nil(t, committed.CompletedAt)
				require.Equal(t, "", committed.CurrentStep)
				require.Nil(t, committed.CurrentFlowID)
				ordinal, ordinalErr := chatGraphVisitOrdinal(&committed)
				require.NoError(t, ordinalErr)
				require.Equal(t, 0, ordinal)
				require.True(t, committed.LastActivityAt.Equal(env.loaded(t, 0)),
					"the committed checkpoint must be the one the graph run loaded")
				require.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1,
					"the body must be durably committed before the handover")
				require.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
				_, ownsOuterTransaction := tx.Statement.ConnPool.(gorm.TxCommitter)
				require.True(t, ownsOuterTransaction, "the handover precheck must use the existing phase-two transaction")
				rootPool, isRootSQLPool := executionAtBarrier.rootApp().DB.Statement.ConnPool.(*sql.DB)
				require.True(t, isRootSQLPool, "root must be the actual SQL pool, not an unwrapped transaction")
				require.Same(t, env.sqlPool, rootPool, "saturate the exact root pool used by the handover acquisition")
				require.Same(t, executionAtBarrier.DB.Statement.ConnPool, tx.Statement.ConnPool, "callback must still own the outer phase-two transaction")
				rootPoolIdentity, callbackOuterIdentity = true, true
				require.NotNil(t, executionAtBarrier.inboundContinuation)
				require.False(t, executionAtBarrier.inboundContinuation.attemptGuarded, "body guard must already have released")
				require.NoError(t, env.pool.Statement.Context.Err(), "do not substitute a caller cancellation for the 15s wait")
				// The 30s lease heartbeat must not need a pooled slot during the 15s hold.
				require.Less(t, claimToBarrier, 10*time.Second)
				require.True(t, barrierUsed.CompareAndSwap(false, true))
				barrierQueries.Add(1)
				preHandoverBarrierArmed = true
				poolStatsBeforeCap = env.sqlPool.Stats()
				poolInUseAtBarrier = poolStatsBeforeCap.InUse
				require.Equal(t, 1, poolInUseAtBarrier, "only phase two may own a private-pool connection here")
				env.sqlPool.SetMaxOpenConns(2)
				// Lowering max-open alone can leave an idle escape slot: sql.DB
				// consumes free connections before checking the max-open bound.
				env.sqlPool.SetMaxIdleConns(0)
				poolStatsAfterCap = env.sqlPool.Stats()
				acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), 5*time.Second)
				held, acquireErr := env.sqlPool.Conn(acquireCtx)
				cancelAcquire()
				require.NoError(t, acquireErr, "reserve the independent handover slot with a bounded test acquisition")
				probe.mu.Lock()
				probe.held, probe.armed, probe.blockedAt = held, true, time.Now()
				probe.timer = time.AfterFunc(45*time.Second, func() { probe.release("watchdog-no-transfer-timeout") })
				probe.mu.Unlock()
				poolStatsAfterHeld = env.sqlPool.Stats()
				require.Equal(t, 2, poolStatsAfterHeld.OpenConnections)
				require.Equal(t, 2, poolStatsAfterHeld.InUse)
				require.Equal(t, 0, poolStatsAfterHeld.Idle)
				require.Equal(t, 2, poolStatsAfterHeld.MaxOpenConnections)
			}))
			t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(callbackName) })

			readState := func() map[string]any {
				state := env.state(t)
				deferrals := h1JobDeferrals(state.Job)
				return map[string]any{
					"session_status": string(state.Session.Status), "session_completed_at_present": state.Session.CompletedAt != nil,
					"session_current_step": state.Session.CurrentStep, "session_path_ordinal": state.Ordinal,
					"session_t1_entries": state.T1Entries,
					"job_status":         string(state.Job.Status), "job_attempts": state.Job.Attempts, "job_deferrals": deferrals,
					"inbound_completed": state.InboundCompleted,
					"active_transfers":  state.ActiveTransfers,
					"provider_calls":    state.ProviderCalls, "outgoing_messages": len(state.Outgoing),
					"action_ledger_rows": state.Ledger, "graph_calls": state.GraphCalls,
				}
			}

			claimStart = time.Now()
			firstErr := env.run(t, env.baseTime)
			probe.release("process-returned-without-timeout-log") // Idempotent; preserves the original release reason.
			first := readState()
			firstSession := env.state(t).Session

			replayErr := env.run(t, env.baseTime.Add(2*time.Second))
			replayed := readState()
			replayState := env.state(t)
			env.mu.Lock()
			graphErrors := append([]string(nil), env.graphErrors...)
			env.mu.Unlock()
			observed := map[string]any{
				"route": route, "fixture_dispatch_only": true, "production_timeout_changed": false,
				"pre_handover_barrier_armed": preHandoverBarrierArmed,
				"barrier_queries":            barrierQueries.Load(), "pool_in_use_before_saturation": poolInUseAtBarrier,
				"claim_to_barrier_ms": claimToBarrier.Milliseconds(),
				"root_pool_identity":  rootPoolIdentity, "callback_outer_transaction_identity": callbackOuterIdentity,
				"pool_stats_before_cap": poolStatsBeforeCap, "pool_stats_after_cap": poolStatsAfterCap,
				"pool_stats_after_held":   poolStatsAfterHeld,
				"agent_transfers_queries": observedQueries,
				"first_error":             fmt.Sprint(firstErr), "replay_error": fmt.Sprint(replayErr), "graph_errors": graphErrors,
				"wait": probe.snapshot(), "after_first": first, "after_replay": replayed,
			}
			raw, err := json.Marshal(observed)
			require.NoError(t, err)
			t.Logf("H1_TRANSFER_TIMEOUT_OBSERVATION %s", raw)

			// Validate the regression's own setup before the durability invariant: a
			// failed setup or a watchdog release is not evidence about the handover.
			require.True(t, preHandoverBarrierArmed)
			require.EqualValues(t, 1, barrierQueries.Load())
			wait := probe.snapshot()
			require.Equal(t, true, wait["actual_timeout_observed"])
			require.Equal(t, "actual-transfer-timeout-log", wait["release_reason"])
			require.Empty(t, wait["release_error"])
			require.GreaterOrEqual(t, wait["blocked_to_timeout_ms"].(int64), int64(15000))
			require.EqualValues(t, 1, first["provider_calls"], "the real body must succeed before the transfer wait")
			require.EqualValues(t, 1, first["outgoing_messages"])

			// After the first run: a retryable, refunded wedge-guard deferral with
			// nothing committed and the graph path ordinal unchanged.
			h1RequireWedgeGuard(t, firstErr)
			assert.Equal(t, string(models.ScheduledJobStatusPending), first["job_status"])
			assert.Equal(t, 0, first["job_attempts"], "a bounded handover wait is a refunded deferral")
			assert.Equal(t, 1, first["job_deferrals"])
			assert.Equal(t, false, first["inbound_completed"])
			assert.EqualValues(t, 0, first["active_transfers"])
			assert.Equal(t, string(models.SessionStatusActive), first["session_status"])
			assert.Equal(t, false, first["session_completed_at_present"])
			assert.Equal(t, 0, first["session_path_ordinal"])
			// runChatGraph's unchanged error path persisted the original active
			// checkpoint on tx A, which commits (nativeBookingCheckpoint is false).
			assert.Equal(t, "t1", first["session_current_step"])
			assert.Nil(t, firstSession.CurrentFlowID)
			assert.EqualValues(t, 1, first["action_ledger_rows"])
			missingFirst := first["active_transfers"].(int64) == 0
			// Session completion alone could be a recoverable intermediate state;
			// the durable inbound obligation must not also be terminal while lost.
			assert.False(t, missingFirst && (first["job_status"] == string(models.ScheduledJobStatusCompleted) ||
				first["inbound_completed"] == true),
				"durability violation: the inbound handover obligation is terminal without an active transfer")

			// After capacity returns and run_at is due: the actual replay hands over once.
			require.NoError(t, replayErr)
			assert.EqualValues(t, 1, replayed["active_transfers"], "same-message recovery after capacity returns must create exactly one handover")
			assert.EqualValues(t, 1, replayed["provider_calls"], "recovery must not resend the body")
			assert.EqualValues(t, 1, replayed["outgoing_messages"])
			assert.EqualValues(t, 1, replayed["action_ledger_rows"])
			assert.EqualValues(t, 2, replayed["graph_calls"])
			assert.Equal(t, string(models.ScheduledJobStatusCompleted), replayed["job_status"])
			assert.Equal(t, true, replayed["inbound_completed"])
			assert.Equal(t, string(models.SessionStatusCompleted), replayed["session_status"])
			assert.Equal(t, true, replayed["session_completed_at_present"])
			assert.Equal(t, 1, replayed["session_t1_entries"])
			transfers := env.transfers(t)
			require.Len(t, transfers, 1)
			assert.Equal(t, models.TransferStatusActive, transfers[0].Status)
			assert.Equal(t, models.TransferSourceFlow, transfers[0].Source)
			assert.NotNil(t, transfers[0].SLA.ResponseDeadline)
			if route == "team" {
				require.NotNil(t, transfers[0].TeamID)
				assert.Equal(t, env.team.ID, *transfers[0].TeamID)
			} else {
				assert.Nil(t, transfers[0].TeamID)
			}
			assert.True(t, replayState.ContactCutoff, "the committed handover cut off automatic replies")
		})
	}
}

// The fast, always-on variant: the handover's own INSERT fails with the exact shape
// of a bounded-wait expiry. Nothing commits, the attempt is refunded, and the replay
// hands over once (one created envelope) without resending the body.
func TestH1GraphTransferHandoverWedgeRetriesWithoutLoss(t *testing.T) {
	for _, route := range []string{"queue", "team"} {
		t.Run(route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route: route, hub: true,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			h1FailFirstCreate(t, env.pool, "h1:handover_wedge:"+route, h1IsAgentTransferCreate,
				h1SyntheticWedge("handover insert"))
			t.Cleanup(func() { _ = env.pool.Callback().Create().Remove("h1:handover_wedge:" + route) })

			firstErr := env.run(t, env.baseTime)
			h1RequireWedgeGuard(t, firstErr)
			first := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
			assert.Equal(t, 0, first.Job.Attempts)
			assert.Equal(t, 1, h1JobDeferrals(first.Job))
			assert.False(t, first.InboundCompleted)
			assert.Zero(t, first.ActiveTransfers)
			assert.Equal(t, models.SessionStatusActive, first.Session.Status)
			assert.Nil(t, first.Session.CompletedAt)
			assert.Equal(t, 0, first.Ordinal)
			assert.Equal(t, "t1", first.Session.CurrentStep)
			assert.False(t, first.ContactCutoff)
			assert.False(t, first.InboundSuppressed)
			assert.EqualValues(t, 1, first.ProviderCalls)
			assert.Len(t, first.Outgoing, 1)
			assert.EqualValues(t, 1, first.Ledger)
			h1RequireEnvelopes(t, env.envelopes, 0)

			require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
			replayed := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
			assert.True(t, replayed.InboundCompleted)
			assert.EqualValues(t, 1, replayed.ActiveTransfers)
			assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
			assert.NotNil(t, replayed.Session.CompletedAt)
			assert.Equal(t, 1, replayed.T1Entries)
			assert.EqualValues(t, 1, replayed.ProviderCalls, "recovery must not resend the body")
			assert.Len(t, replayed.Outgoing, 1)
			assert.EqualValues(t, 1, replayed.Ledger)
			assert.EqualValues(t, 2, replayed.GraphCalls)
			transfers := env.transfers(t)
			require.Len(t, transfers, 1)
			assert.Equal(t, models.TransferSourceFlow, transfers[0].Source)
			assert.NotNil(t, transfers[0].SLA.ResponseDeadline)
			if route == "team" {
				require.NotNil(t, transfers[0].TeamID)
				assert.Equal(t, env.team.ID, *transfers[0].TeamID)
			} else {
				assert.Nil(t, transfers[0].TeamID)
			}
			h1RequireEnvelopes(t, env.envelopes, 1)
		})
	}
}

// The team route's selected agent, SLA pickup and AssignToSameAgent are applied once,
// on the committed transfer. AssignToTeam itself runs on tx A, which commits its
// last_assigned_at even when the handover fails: the replay selects again (recorded
// round-robin drift, a documented residual).
func TestH1GraphTransferTeamSelectedAgentAppliedOnce(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		route: "team", hub: true,
		flowCfg: map[string]any{"body": "Connecting you to a human."},
		settings: func(settings *models.ChatbotSettings) {
			settings.AgentAssignment.AssignToSameAgent = true
		},
	})
	if env.app.Redis == nil {
		t.Skip("TEST_REDIS_URL not set: the team Assigner caches team configuration in Redis")
	}
	member := testutil.CreateTestUser(t, env.shared, env.org.ID)
	require.NoError(t, env.shared.Create(&models.TeamMember{
		BaseModel: models.BaseModel{ID: uuid.New()}, TeamID: env.team.ID,
		UserID: member.ID, Role: models.TeamRoleAgent,
	}).Error)
	env.app.Assigner = assignment.New(env.pool, env.app.Redis, env.app.Log)
	lastAssigned := func() *time.Time {
		var teamMember models.TeamMember
		require.NoError(t, env.shared.Where("team_id = ? AND user_id = ?", env.team.ID, member.ID).First(&teamMember).Error)
		return teamMember.LastAssignedAt
	}
	require.Nil(t, lastAssigned())
	h1FailFirstCreate(t, env.pool, "h1:team_handover_wedge", h1IsAgentTransferCreate,
		h1SyntheticWedge("handover insert"))
	t.Cleanup(func() { _ = env.pool.Callback().Create().Remove("h1:team_handover_wedge") })

	h1RequireWedgeGuard(t, env.run(t, env.baseTime))
	first := env.state(t)
	assert.Zero(t, first.ActiveTransfers)
	assert.Nil(t, first.ContactAssignee, "no assignment without the committed handover")
	firstAssigned := lastAssigned()
	require.NotNil(t, firstAssigned, "AssignToTeam ran on tx A, which committed on the error path")
	h1RequireEnvelopes(t, env.envelopes, 0)

	time.Sleep(5 * time.Millisecond) // a distinct round-robin timestamp on replay
	require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
	replayed := env.state(t)
	transfers := env.transfers(t)
	require.Len(t, transfers, 1)
	require.NotNil(t, transfers[0].AgentID)
	assert.Equal(t, member.ID, *transfers[0].AgentID)
	require.NotNil(t, transfers[0].TeamID)
	assert.Equal(t, env.team.ID, *transfers[0].TeamID)
	assert.NotNil(t, transfers[0].SLA.PickedUpAt)
	assert.NotNil(t, transfers[0].SLA.ResponseDeadline)
	require.NotNil(t, replayed.ContactAssignee)
	assert.Equal(t, member.ID, *replayed.ContactAssignee)
	assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
	assert.EqualValues(t, 1, replayed.ProviderCalls)
	h1RequireEnvelopes(t, env.envelopes, 1)
	replayAssigned := lastAssigned()
	require.NotNil(t, replayAssigned)
	assert.True(t, replayAssigned.After(*firstAssigned), "documented drift: the replay re-runs AssignToTeam")
}

// h1FailCutoffAfterCompletion counts the owned-session completion CAS on db and fails
// the first CutoffTx contact read that follows the first CAS, once.
func h1FailCutoffAfterCompletion(t *testing.T, db *gorm.DB, name string, failure error) *atomic.Int32 {
	t.Helper()
	var completions atomic.Int32
	var armed atomic.Bool
	armed.Store(true)
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name+":count", func(tx *gorm.DB) {
		if h1IsSessionCompletionUpdate(tx) {
			completions.Add(1)
		}
	}))
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name+":fail", func(tx *gorm.DB) {
		if completions.Load() == 1 && h1IsCutoffContactRead(tx) && armed.CompareAndSwap(true, false) {
			_ = tx.AddError(failure)
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Update().Remove(name + ":count")
		_ = db.Callback().Query().Remove(name + ":fail")
	})
	return &completions
}

// A failure after the completion CAS but before COMMIT rolls back both the transfer
// and the completion; the caller's session is untouched and the replay hands over.
func TestExecChatTransfer_FailureAfterCompletionCASRollsBackBoth(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		flowCfg: map[string]any{"body": "Connecting you to a human."},
	})
	completions := h1FailCutoffAfterCompletion(t, env.pool, "h1:cutoff_after_cas",
		errors.New("h1 synthetic cutoff failure"))

	err := env.run(t, env.baseTime)
	require.Error(t, err)
	require.ErrorContains(t, err, "h1 synthetic cutoff failure")
	assert.False(t, inboundContinuationStoppedByPolicy(err))
	assert.EqualValues(t, 1, completions.Load(), "the completion CAS ran inside the handover before the failure")
	first := env.state(t)
	assert.Zero(t, first.ActiveTransfers)
	assert.Equal(t, models.SessionStatusActive, first.Session.Status)
	assert.Nil(t, first.Session.CompletedAt)
	assert.Zero(t, first.T1Entries)
	assert.False(t, first.ContactCutoff, "CutoffTx's epoch must roll back with the handover")
	assert.False(t, first.InboundSuppressed)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
	assert.Equal(t, 1, first.Job.Attempts, "an ordinary failure consumes its attempt")
	inMemory := env.current.Load()
	require.NotNil(t, inMemory)
	assert.Equal(t, models.SessionStatusActive, inMemory.Status, "the caller's session is published only after commit")
	assert.Nil(t, inMemory.CompletedAt)
	assert.Zero(t, h1PathEntries(inMemory, "t1"))

	require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
	replayed := env.state(t)
	assert.EqualValues(t, 1, replayed.ActiveTransfers)
	assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
	assert.Equal(t, 1, replayed.T1Entries)
	assert.EqualValues(t, 1, replayed.ProviderCalls)
	assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
}

// canonicalContactWriteTransaction retries the handover closure; the completion
// checkpoint is rebuilt from the untouched session on every attempt (a reused staged
// copy would carry a rolled-back LastActivityAt, lose its CAS and append t1 twice).
func TestExecChatTransfer_CanonicalRetryRebuildsCompletionCheckpoint(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		flowCfg: map[string]any{"body": "Connecting you to a human."},
	})
	completions := h1FailCutoffAfterCompletion(t, env.pool, "h1:cutoff_deadlock",
		&pgconn.PgError{Code: "40P01", Message: "h1 synthetic deadlock"})

	require.NoError(t, env.run(t, env.baseTime))
	assert.EqualValues(t, 2, completions.Load(), "the completion CAS ran once per canonical-write attempt")
	state := env.state(t)
	assert.EqualValues(t, 1, state.ActiveTransfers)
	assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
	assert.NotNil(t, state.Session.CompletedAt)
	assert.Equal(t, 1, state.T1Entries, "exactly one t1 path entry")
	assert.True(t, state.Session.LastActivityAt.After(env.loaded(t, 0)))
	assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
	assert.True(t, state.InboundCompleted)
	inMemory := env.current.Load()
	require.NotNil(t, inMemory)
	assert.Equal(t, models.SessionStatusCompleted, inMemory.Status)
	assert.True(t, inMemory.LastActivityAt.Equal(state.Session.LastActivityAt),
		"the published session is the committed attempt's checkpoint")
	assert.Equal(t, 1, h1PathEntries(inMemory, "t1"))
}

// The independent handover commits, then the outer settlement is lost. The replay
// is terminal without a second transfer, created notification or body.
func TestH1GraphTransferLostSettlementAfterHandover(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		hub:     true,
		flowCfg: map[string]any{"body": "Connecting you to a human."},
	})
	var armed atomic.Bool
	armed.Store(true)
	require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register("h1:lost_settlement", func(tx *gorm.DB) {
		if !env.graphReturned.Load() {
			return
		}
		if _, isMessage := tx.Statement.Dest.(*models.Message); !isMessage {
			return
		}
		if _, locked := tx.Statement.Clauses["FOR"]; !locked {
			return
		}
		if armed.CompareAndSwap(true, false) {
			_ = tx.AddError(errors.New("h1 synthetic lost settlement"))
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Query().Remove("h1:lost_settlement") })

	err := env.run(t, env.baseTime)
	require.ErrorContains(t, err, "h1 synthetic lost settlement")
	first := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
	assert.False(t, first.InboundCompleted)
	assert.EqualValues(t, 1, first.ActiveTransfers, "the independent handover committed")
	assert.Equal(t, models.SessionStatusCompleted, first.Session.Status)
	assert.Equal(t, 1, first.T1Entries)
	h1RequireEnvelopes(t, env.envelopes, 1)

	require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
	replayed := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
	assert.True(t, replayed.InboundCompleted)
	assert.EqualValues(t, 1, replayed.ActiveTransfers)
	assert.Len(t, env.transfers(t), 1)
	assert.EqualValues(t, 1, replayed.ProviderCalls)
	assert.Len(t, replayed.Outgoing, 1)
	assert.EqualValues(t, 1, replayed.Ledger)
	assert.Equal(t, 1, replayed.T1Entries)
	h1RequireEnvelopes(t, env.envelopes, 1)
}

// An already-active transfer (here a raw row created without CutoffTx) is proven
// under the canonical locks: only the owned session completes. Nothing is created,
// cut off, reassigned, re-SLA'd or announced, and the hours decline is skipped.
func TestExecChatTransfer_ExistingActiveTransferCompletesOwnedSession(t *testing.T) {
	for _, route := range []string{"queue", "team"} {
		t.Run(route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route: route, hub: true,
				settings: func(settings *models.ChatbotSettings) {
					settings.BusinessHours = models.BusinessHoursConfig{
						Enabled: true, Hours: h1AllDaysClosed(), OutOfHoursMessage: "We are closed",
					}
				},
			})
			agent := testutil.CreateTestUser(t, env.shared, env.org.ID)
			var member *models.User
			if route == "team" && env.app.Redis != nil {
				member = testutil.CreateTestUser(t, env.shared, env.org.ID)
				require.NoError(t, env.shared.Create(&models.TeamMember{
					BaseModel: models.BaseModel{ID: uuid.New()}, TeamID: env.team.ID,
					UserID: member.ID, Role: models.TeamRoleAgent,
				}).Error)
				env.app.Assigner = assignment.New(env.pool, env.app.Redis, env.app.Log)
			}
			existing := models.AgentTransfer{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
				ContactID: env.work.Contact.ID, WhatsAppAccount: env.account.Name,
				PhoneNumber: env.work.Contact.PhoneNumber, Status: models.TransferStatusActive,
				Source: models.TransferSourceManual, AgentID: &agent.ID, TransferredAt: time.Now().UTC(),
			}
			require.NoError(t, env.shared.Create(&existing).Error)

			require.NoError(t, env.run(t, env.baseTime))
			state := env.state(t)
			transfers := env.transfers(t)
			require.Len(t, transfers, 1)
			assert.Equal(t, existing.ID, transfers[0].ID)
			require.NotNil(t, transfers[0].AgentID)
			assert.Equal(t, agent.ID, *transfers[0].AgentID)
			assert.Nil(t, transfers[0].TeamID)
			assert.Nil(t, transfers[0].SLA.ResponseDeadline)
			assert.Nil(t, transfers[0].SLA.PickedUpAt)
			assert.Zero(t, state.ProviderCalls, "no out-of-hours reply when a handover already exists")
			assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
			assert.Equal(t, 1, state.T1Entries)
			assert.False(t, state.ContactCutoff, "an existing transfer is never cut off again")
			assert.Nil(t, state.ContactAssignee)
			assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
			h1RequireEnvelopes(t, env.envelopes, 0)
			if member != nil {
				var teamMember models.TeamMember
				require.NoError(t, env.shared.Where("team_id = ? AND user_id = ?", env.team.ID, member.ID).First(&teamMember).Error)
				assert.Nil(t, teamMember.LastAssignedAt, "AssignToTeam is not called for an existing transfer")
			}
		})
	}
}

// h1AfterOuterAdvisoryCount runs fn once, right after the graph handover's advisory
// Count on the outer transaction returned.
func (env *h1HandoverEnv) h1AfterOuterAdvisoryCount(t *testing.T, name string, fn func()) *atomic.Bool {
	t.Helper()
	var ran atomic.Bool
	require.NoError(t, env.pool.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if env.h1IsOuterAdvisoryCount(tx) && ran.CompareAndSwap(false, true) {
			fn()
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
	return &ran
}

// A transfer that appears after the advisory read is proven (raw row) or wins (a real
// CutoffTx handover cancels the owned session: a terminal policy stop). There is
// never a second transfer or a second created notification.
func TestExecChatTransfer_ConcurrentTransferAfterPrecheck(t *testing.T) {
	t.Run("raw_active_transfer_is_proven", func(t *testing.T) {
		env := h1HandoverFixture(t, h1HandoverOptions{hub: true})
		ran := env.h1AfterOuterAdvisoryCount(t, "h1:concurrent_raw", func() {
			require.NoError(t, env.shared.Create(&models.AgentTransfer{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
				ContactID: env.work.Contact.ID, WhatsAppAccount: env.account.Name,
				PhoneNumber: env.work.Contact.PhoneNumber, Status: models.TransferStatusActive,
				Source: models.TransferSourceManual, TransferredAt: time.Now().UTC(),
			}).Error)
		})
		require.NoError(t, env.run(t, env.baseTime))
		require.True(t, ran.Load())
		state := env.state(t)
		assert.EqualValues(t, 1, state.ActiveTransfers)
		assert.Len(t, env.transfers(t), 1)
		assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
		assert.Equal(t, 1, state.T1Entries)
		assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
		h1RequireEnvelopes(t, env.envelopes, 0)
	})

	t.Run("real_handover_wins", func(t *testing.T) {
		env := h1HandoverFixture(t, h1HandoverOptions{hub: true})
		var writerTransfer models.AgentTransfer
		ran := env.h1AfterOuterAdvisoryCount(t, "h1:concurrent_handover", func() {
			account := *env.account
			contact := env.work.Contact
			writerTransfer = models.AgentTransfer{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: env.org.ID,
				ContactID: contact.ID, WhatsAppAccount: account.Name, PhoneNumber: contact.PhoneNumber,
				Status: models.TransferStatusActive, Source: models.TransferSourceManual,
				TransferredAt: time.Now().UTC(),
			}
			require.NoError(t, env.app.WithCommittedTenantApp(env.org.ID, func(scoped *App) error {
				return scoped.saveAndFinalizeTransfer(&writerTransfer, &account, &contact, nil, true, nil)
			}))
			require.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID),
				"the concurrent writer's handover committed")
		})
		require.NoError(t, env.run(t, env.baseTime), "a lost ownership is a terminal policy stop")
		require.True(t, ran.Load())
		env.mu.Lock()
		graphErrors := append([]string(nil), env.graphErrors...)
		env.mu.Unlock()
		require.Len(t, graphErrors, 1)
		assert.Contains(t, graphErrors[0], "transfer node handover")
		state := env.state(t)
		transfers := env.transfers(t)
		require.Len(t, transfers, 1)
		assert.Equal(t, writerTransfer.ID, transfers[0].ID)
		assert.Equal(t, models.SessionStatusCancelled, state.Session.Status, "the writer's handover cancelled the session")
		assert.Zero(t, state.T1Entries)
		assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
		assert.True(t, state.InboundCompleted)
		h1RequireEnvelopes(t, env.envelopes, 1)
	})
}

// Pins the durability critic's residual (a): a writer that advances the session
// WITHOUT FOR UPDATE (compatible with tx A's FK KEY SHARE) after the body was sent
// makes the runner's CAS a terminal policy stop with no transfer. Production
// acquisitions take FOR UPDATE and wait for tx A instead.
func TestExecChatTransfer_StaleOwnershipAfterBodyHandsOverNothing(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		flowCfg: map[string]any{"body": "Connecting you to a human."},
	})
	var bumped time.Time
	ran := env.h1AfterOuterAdvisoryCount(t, "h1:stale_bump", func() {
		require.NoError(t, env.shared.Exec(
			"UPDATE chatbot_sessions SET last_activity_at = last_activity_at + interval '1 second' WHERE id = ? AND organization_id = ?",
			env.session.ID, env.org.ID,
		).Error)
		var stored models.ChatbotSession
		require.NoError(t, env.shared.Where("id = ?", env.session.ID).First(&stored).Error)
		bumped = stored.LastActivityAt
	})
	require.NoError(t, env.run(t, env.baseTime), "lost ownership is a terminal policy stop")
	require.True(t, ran.Load())
	state := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
	assert.True(t, state.InboundCompleted)
	assert.Zero(t, state.ActiveTransfers)
	assert.EqualValues(t, 1, state.ProviderCalls)
	assert.Len(t, state.Outgoing, 1)
	assert.Equal(t, models.SessionStatusActive, state.Session.Status)
	assert.Nil(t, state.Session.CompletedAt)
	assert.Equal(t, "", state.Session.CurrentStep, "the error-path persist lost its CAS as well")
	assert.True(t, state.Session.LastActivityAt.Equal(bumped))
}

// A session/contact of another organization than the account is rejected before the
// body: no provider call, no writes, an ordinary (counted) failure.
func TestExecChatTransfer_ForeignOrganizationRejectedBeforeBody(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{
		foreignAccount: true,
		flowCfg:        map[string]any{"body": "Connecting you to a human."},
	})
	err := env.run(t, env.baseTime)
	require.Error(t, err)
	assert.False(t, inboundContinuationStoppedByPolicy(err))
	assert.NotContains(t, err.Error(), "wedge guard")
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "organizations differ")
	state := env.state(t)
	assert.Zero(t, state.ProviderCalls)
	assert.Zero(t, state.ActiveTransfers)
	var foreignTransfers int64
	require.NoError(t, env.shared.Model(&models.AgentTransfer{}).Where("organization_id = ?", env.foreignOrg.ID).Count(&foreignTransfers).Error)
	assert.Zero(t, foreignTransfers)
	assert.Equal(t, models.SessionStatusActive, state.Session.Status)
	assert.Nil(t, state.Session.CompletedAt)
	assert.Zero(t, state.T1Entries)
	assert.Equal(t, models.ScheduledJobStatusPending, state.Job.Status)
	assert.Equal(t, 1, state.Job.Attempts)
}

// Pins the durability critic's residual (b) through real dispatch: after a failed
// handover, a later inbound from the same contact re-runs t1 under ITS action keys
// (the body is sent again) and hands over exactly once; the earlier inbound is
// suppressed by CutoffTx and its replay is terminal.
func TestH1GraphTransferLaterInboundResendsBodyAfterFailedHandover(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	require.NoError(t, shared.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		WhatsAppAccount: account.Name, IsEnabled: true, SessionTimeoutMins: 30,
		AI: models.AIConfig{Enabled: false},
	}).Error)
	flow := &models.ChatbotFlow{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
		WhatsAppAccount: account.Name, Name: "h1-human-handover", IsEnabled: true,
		TriggerKeywords: models.StringArray{"human"},
		Graph: models.JSONB{
			"version": 2, "entry_node": "t1",
			"nodes": []any{map[string]any{"id": "t1", "type": "transfer", "label": "handoff",
				"config": map[string]any{"body": "Connecting you to a human."}}},
			"edges": []any{},
		},
	}
	require.NoError(t, shared.Create(flow).Error)
	phone := "6063" + uuid.NewString()[:8]
	first, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.h1-later-1-"+uuid.NewString(), phone, "human"), "Later patient")
	require.NoError(t, err)
	require.False(t, duplicate)

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	calls := h1CountingProvider(t, app, http.StatusOK)
	envelopes := h1CountTransferEnvelopes(t, app, organization.ID)
	h1FailFirstCreate(t, pool, "h1:later_inbound_wedge", h1IsAgentTransferCreate, h1SyntheticWedge("handover insert"))
	t.Cleanup(func() { _ = pool.Callback().Create().Remove("h1:later_inbound_wedge") })
	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }

	h1RequireWedgeGuard(t, processor.ProcessMessage(context.Background(), organization.ID, first.Persisted.ID))
	var session models.ChatbotSession
	require.NoError(t, shared.Where("organization_id = ? AND contact_id = ?", organization.ID, first.Contact.ID).First(&session).Error)
	assert.Equal(t, models.SessionStatusActive, session.Status)
	require.NotNil(t, session.CurrentFlowID)
	assert.Equal(t, flow.ID, *session.CurrentFlowID)
	assert.Equal(t, "t1", session.CurrentStep)
	assert.EqualValues(t, 1, calls.Load())
	job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, first.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 1, h1JobDeferrals(job))

	// Persist the later inbound through the fixture pool, exactly as the first one
	// (this unit schema has no production phone resolver for the RLS admission path).
	rlsConfig := app.Config
	app.DB, app.Config = shared, nil
	second, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.h1-later-2-"+uuid.NewString(), phone, "still there?"), "Later patient")
	app.DB, app.Config = pool, rlsConfig
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, first.Contact.ID, second.Contact.ID)
	processor.now = func() time.Time { return time.Now().UTC() }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, second.Persisted.ID))
	assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, organization.ID, first.Contact.ID))
	assert.EqualValues(t, 2, calls.Load(), "documented residual: the later inbound resends the body under its own keys")
	assert.Len(t, h1OutgoingMessages(t, shared, organization.ID, first.Contact.ID), 2)
	h1RequireEnvelopes(t, envelopes, 1)
	require.NoError(t, shared.Where("id = ?", session.ID).First(&session).Error)
	assert.Equal(t, models.SessionStatusCompleted, session.Status)
	var firstInbound models.Message
	require.NoError(t, shared.Where("id = ?", first.Persisted.ID).First(&firstInbound).Error)
	assert.Equal(t, true, firstInbound.Metadata[incomingAutomaticAISuppressedKey], "CutoffTx suppressed the earlier inbound")

	processor.now = func() time.Time { return time.Now().UTC().Add(2 * time.Second) }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, first.Persisted.ID))
	job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, first.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, organization.ID, first.Contact.ID))
	assert.EqualValues(t, 2, calls.Load())
	h1RequireEnvelopes(t, envelopes, 1)
}

// The intentional business-hours decline: the out-of-hours reply is sent (after the
// body) and must succeed before the owned session completes; no transfer is created.
func TestExecChatTransfer_BusinessHoursDecline(t *testing.T) {
	closed := func(settings *models.ChatbotSettings) {
		settings.BusinessHours = models.BusinessHoursConfig{
			Enabled: true, Hours: h1AllDaysClosed(), OutOfHoursMessage: "We are closed",
		}
	}
	options := h1HandoverOptions{
		settings: closed,
		flowCfg:  map[string]any{"body": "Connecting you to a human."},
	}
	requireDeclined := func(t *testing.T, env *h1HandoverEnv, providerCalls int32) {
		t.Helper()
		state := env.state(t)
		assert.EqualValues(t, providerCalls, state.ProviderCalls)
		assert.Zero(t, state.ActiveTransfers)
		assert.Empty(t, env.transfers(t))
		assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
		assert.NotNil(t, state.Session.CompletedAt)
		assert.Equal(t, 1, state.T1Entries)
		assert.False(t, state.ContactCutoff)
		assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
		contents := make([]string, 0, len(state.Outgoing))
		for _, message := range state.Outgoing {
			contents = append(contents, message.Content)
		}
		assert.Equal(t, []string{"Connecting you to a human.", "We are closed"}, contents)
	}

	t.Run("decline_completes_after_reply", func(t *testing.T) {
		env := h1HandoverFixture(t, options)
		require.NoError(t, env.run(t, env.baseTime))
		requireDeclined(t, env, 2)
	})

	t.Run("reply_pre_dispatch_failure_is_retried", func(t *testing.T) {
		env := h1HandoverFixture(t, options)
		var ledgerCreates atomic.Int32
		require.NoError(t, env.pool.Callback().Create().Before("gorm:create").Register("h1:decline_reply_ledger", func(tx *gorm.DB) {
			if h1IsActionLedgerCreate(tx) && ledgerCreates.Add(1) == 2 {
				_ = tx.AddError(context.DeadlineExceeded)
			}
		}))
		t.Cleanup(func() { _ = env.pool.Callback().Create().Remove("h1:decline_reply_ledger") })
		err := env.run(t, env.baseTime)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		state := env.state(t)
		assert.Equal(t, models.SessionStatusActive, state.Session.Status)
		assert.Equal(t, 0, state.Ordinal)
		assert.Nil(t, state.Session.CompletedAt)
		assert.EqualValues(t, 1, state.ProviderCalls)
		assert.Equal(t, models.ScheduledJobStatusPending, state.Job.Status)

		require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
		requireDeclined(t, env, 2)
	})

	t.Run("decline_completion_wedge_is_retried", func(t *testing.T) {
		env := h1HandoverFixture(t, options)
		var armed atomic.Bool
		armed.Store(true)
		require.NoError(t, env.pool.Callback().Update().Before("gorm:update").Register("h1:decline_completion_wedge", func(tx *gorm.DB) {
			if h1IsSessionCompletionUpdate(tx) && armed.CompareAndSwap(true, false) {
				_ = tx.AddError(h1SyntheticWedge("decline completion"))
			}
		}))
		t.Cleanup(func() { _ = env.pool.Callback().Update().Remove("h1:decline_completion_wedge") })
		err := env.run(t, env.baseTime)
		h1RequireWedgeGuard(t, err)
		state := env.state(t)
		assert.Equal(t, models.SessionStatusActive, state.Session.Status)
		assert.Nil(t, state.Session.CompletedAt)
		assert.EqualValues(t, 2, state.ProviderCalls)
		assert.Equal(t, models.ScheduledJobStatusPending, state.Job.Status)
		assert.Equal(t, 1, h1JobDeferrals(state.Job))

		require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
		requireDeclined(t, env, 2)
		assert.EqualValues(t, 2, env.state(t).Ledger, "body and reply were replayed from the ledger")
	})
}
