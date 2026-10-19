package handlers

// H1 regressions from the round-1 liveness and durability reviews of df4c6c16.
//
// (1) The nested graph handover's organization fence (advisory lock, then
// organizations FOR UPDATE) used to run under the post-fence SET LOCAL
// lock_timeout = '3s'. That was wrong both ways. Under overlapping in-flight AI
// attempts of other conversations (each holds the organization row FOR KEY SHARE,
// and PostgreSQL lets new KEY SHARE lockers pass a queued FOR UPDATE) no single lock
// wait ever reached 3s, so the handover waited without limit while pinning two
// pooled connections and the fence. And one ordinary in-flight attempt of more than
// about 19s made every canonical-write attempt fail, turning a handover that would
// have succeeded into a deferral (and, if the customer wrote again before the retry,
// a second transfer body). The fence now runs under one 40s context deadline plus a statement_timeout budget
// shared by all attempts, and lock_timeout = '3s' applies only after the fence.
//
// (2) The team route's round-robin selection used to UPDATE team_members on the
// continuation transaction and keep that row lock across the handover's own
// connection wait; concurrent same-team handovers then queued behind it in
// PostgreSQL while each pinned its own connection. The selection now commits in a
// short transaction of its own.
//
// Every case drives real ProcessMessage settlement on a dedicated pool (h1OpenPool);
// callbacks are registered only on that pool. None of these tests calls t.Parallel.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// h1FenceStatementTimeout observes the statement_timeout in effect on the
// organizations FOR UPDATE of every fence on db, and fails the first one that runs
// under a non-default statement_timeout (only the nested handover's bounded fence
// does) exactly as an expired statement_timeout does (SQLSTATE 57014), when fail is set.
func h1FenceStatementTimeout(t *testing.T, db *gorm.DB, name string, fail bool) (*atomic.Int32, *atomic.Value) {
	t.Helper()
	var failed atomic.Int32
	var observed atomic.Value
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		statement := tx.Statement.SQL.String()
		if !strings.Contains(statement, "FROM organizations") || !strings.Contains(statement, "FOR UPDATE") {
			return
		}
		var setting string
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SHOW statement_timeout").Row().Scan(&setting); err != nil || setting == "0" {
			return
		}
		observed.Store(setting)
		if fail && failed.CompareAndSwap(0, 1) {
			_ = tx.AddError(&pgconn.PgError{Code: "57014", Severity: "ERROR", Message: "canceling statement due to statement timeout"})
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Row().Remove(name) })
	return &failed, &observed
}

// h1TransferInsertTimeouts records statement_timeout and lock_timeout in effect on
// the handover's own transaction at its agent_transfers insert (after the fence).
func h1TransferInsertTimeouts(t *testing.T, db *gorm.DB, name string) (*atomic.Value, *atomic.Value) {
	t.Helper()
	var statementTimeout, lockTimeout atomic.Value
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if !h1IsAgentTransferCreate(tx) {
			return
		}
		var setting string
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SHOW statement_timeout").Row().Scan(&setting); err == nil {
			statementTimeout.Store(setting)
		}
		if err := tx.Session(&gorm.Session{NewDB: true}).Raw("SHOW lock_timeout").Row().Scan(&setting); err == nil {
			lockTimeout.Store(setting)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(name) })
	return &statementTimeout, &lockTimeout
}

func h1RequireFenceBudgetDeferral(t *testing.T, err error) {
	t.Helper()
	h1RequireWedgeGuard(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the real fence deadline stays inspectable")
	assert.False(t, inboundContinuationRequiresManualReview(err))
	assert.False(t, inboundContinuationStoppedByPolicy(err))
}

func h1RequireFenceBudget(t *testing.T, observed *atomic.Value) {
	t.Helper()
	setting, _ := observed.Load().(string)
	require.NotEmpty(t, setting, "the nested handover's fence must run under a statement_timeout")
	budget, err := time.ParseDuration(setting)
	require.NoError(t, err, "statement_timeout %q", setting)
	assert.Greater(t, budget, 30*time.Second, "the fence budget outlasts one maximal provider attempt")
	assert.LessOrEqual(t, budget, 40*time.Second, "the fence budget stays below the 45s in-fence post-claim wait")
}

// The fence runs under its wall-clock budget, the budget never outlives the fence
// (lock_timeout bounds only the post-fence row waits), and an expired budget (57014)
// is a refunded wedge-guard deferral that is not retried, with nothing committed; the
// replay hands over exactly once without resending the body.
func TestH1HandoverFenceBudgetExpiryIsRefundedDeferral(t *testing.T) {
	for _, route := range []string{"queue", "team"} {
		t.Run("graph_"+route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route:   route,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			failed, fenceBudget := h1FenceStatementTimeout(t, env.pool, "h1:handover_fence_budget:"+route, true)
			insertStatementTimeout, insertLockTimeout := h1TransferInsertTimeouts(t, env.pool, "h1:handover_insert_timeouts:"+route)

			firstErr := env.run(t, env.baseTime)
			h1RequireFenceBudgetDeferral(t, firstErr)
			assert.EqualValues(t, 1, failed.Load(), "an expired fence budget is not retried")
			h1RequireFenceBudget(t, fenceBudget)
			assert.Nil(t, insertStatementTimeout.Load(), "nothing was inserted after the expired fence")
			first := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
			assert.Equal(t, 0, first.Job.Attempts, "an expired fence budget is a refunded deferral")
			assert.Equal(t, 1, h1JobDeferrals(first.Job))
			assert.False(t, first.InboundCompleted)
			assert.Zero(t, first.ActiveTransfers)
			assert.Equal(t, models.SessionStatusActive, first.Session.Status)
			assert.Nil(t, first.Session.CompletedAt)
			assert.Equal(t, 0, first.Ordinal)
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
			// After the fence: the budget is gone and only the row waits are bounded.
			assert.Equal(t, "0", insertStatementTimeout.Load(), "the fence budget must not outlive the fence")
			assert.Equal(t, "3s", insertLockTimeout.Load(), "post-fence lock waits stay bounded")

			// SET LOCAL ended with the handover transaction.
			var statementTimeout, lockTimeout string
			require.NoError(t, env.pool.Raw("SHOW statement_timeout").Row().Scan(&statementTimeout))
			require.NoError(t, env.pool.Raw("SHOW lock_timeout").Row().Scan(&lockTimeout))
			assert.Equal(t, "0", statementTimeout)
			assert.Equal(t, "0", lockTimeout)
			h1RequireLiveAfterRun(t, env)
		})
	}

	t.Run("chatbot_disabled", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, models.BusinessHoursConfig{})
		failed, fenceBudget := h1FenceStatementTimeout(t, app.DB, "h1:disabled_fence_budget", true)
		baseTime := time.Now().UTC()
		processor := NewInboundContinuationProcessor(app, time.Second)
		processor.now = func() time.Time { return baseTime }

		err := processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
		require.ErrorContains(t, err, "create chatbot-disabled queue transfer")
		h1RequireFenceBudgetDeferral(t, err)
		assert.EqualValues(t, 1, failed.Load())
		h1RequireFenceBudget(t, fenceBudget)
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

// h1HoldAIAttemptKeyShare holds the organization row FOR KEY SHARE for holdFor on
// the fixture pool, as another conversation's in-flight AI attempt does
// (database.LockOrganizationAIAttemptScope around its provider call).
func h1HoldAIAttemptKeyShare(env *h1HandoverEnv, holdFor time.Duration, acquired chan<- error, done *sync.WaitGroup) {
	done.Add(1)
	go func() {
		defer done.Done()
		holder := env.shared.Begin()
		if holder.Error != nil {
			acquired <- holder.Error
			return
		}
		defer holder.Rollback()
		if err := database.LockOrganizationAIAttemptScope(holder, env.org.ID); err != nil {
			acquired <- err
			return
		}
		acquired <- nil
		time.Sleep(holdFor)
	}()
}

// One ordinary in-flight AI attempt of another conversation of the organization
// (25s, beyond the old ~19s lock_timeout window, within the App HTTP client's 30s)
// overlaps the pre-handover boundary. The handover must wait it out and commit in
// its first round, with no deferral; a follow-up message of the same customer that
// arrives meanwhile is admitted behind the handover and must not get a second body.
func TestH1GraphTransferHandoverWaitsOutInFlightAIAttempt(t *testing.T) {
	h1RequireSlow(t)
	for _, route := range []string{"queue", "team"} {
		t.Run(route, func(t *testing.T) {
			env := h1HandoverFixture(t, h1HandoverOptions{
				route:   route,
				flowCfg: map[string]any{"body": "Connecting you to a human."},
			})
			// The session is the flow's, so a follow-up processed while it is still
			// active would run the transfer node (and its body) again.
			require.NoError(t, env.shared.Model(&models.ChatbotSession{}).Where("id = ?", env.session.ID).
				Updates(map[string]any{"current_flow_id": env.flow.ID, "current_step": "t1"}).Error)

			const holdFor = 25 * time.Second
			later := inboundContinuationTextMessage(t, "wamid.h1-follow-up-"+uuid.NewString(),
				env.work.Contact.PhoneNumber, "still there?")
			admitter := h1PlainApp(env)
			var holders, admission sync.WaitGroup
			var holderErr, admitErr error
			var boundaryAt time.Time
			var followUp *persistedIncomingMessage
			ran := env.h1AfterOuterAdvisoryCount(t, "h1:in_flight_ai_attempt:"+route, func() {
				boundaryAt = time.Now()
				acquired := make(chan error, 1)
				h1HoldAIAttemptKeyShare(env, holdFor, acquired, &holders)
				holderErr = <-acquired // the handover starts only once the attempt is in flight
				admission.Add(1)
				go func() {
					defer admission.Done()
					time.Sleep(3 * time.Second) // the customer writes again while the handover waits
					followUp, _, admitErr = admitter.persistIncomingMessageBeforeAck(env.account.PhoneID, later, "H1 handover patient")
				}()
			})

			elapsed, runErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
			require.True(t, ran.Load())
			require.NoError(t, holderErr, "the in-flight AI attempt must hold the organization row")
			sinceBoundary := time.Since(boundaryAt)
			t.Logf("H1_HANDOVER_IN_FLIGHT_AI route=%s run=%s since_boundary=%s err=%v", route, elapsed, sinceBoundary, runErr)
			require.NoError(t, runErr, "a single in-flight AI attempt must make the handover wait, not defer")
			assert.GreaterOrEqual(t, sinceBoundary, holdFor-time.Second, "the handover waited for the attempt")
			assert.Less(t, sinceBoundary, 40*time.Second)

			first := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusCompleted, first.Job.Status)
			assert.Zero(t, h1JobDeferrals(first.Job), "one round, no deferral")
			assert.True(t, first.InboundCompleted)
			assert.EqualValues(t, 1, first.ActiveTransfers)
			assert.Equal(t, models.SessionStatusCompleted, first.Session.Status)
			assert.Equal(t, 1, first.T1Entries)
			assert.True(t, first.ContactCutoff)
			assert.EqualValues(t, 1, first.ProviderCalls)
			assert.Len(t, first.Outgoing, 1)

			waited := make(chan struct{})
			go func() { admission.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(60 * time.Second):
				t.Fatalf("the follow-up message was never admitted\n%s", h1PoolSessionReport(env.shared))
			}
			// Admission has its own 20s fence deadline. The first delivery rolls
			// back while this 25s attempt is held, then Meta can retry the same WAMID.
			require.Error(t, admitErr)
			require.Nil(t, followUp)
			var refusedRows int64
			require.NoError(t, env.shared.Model(&models.Message{}).Where(&models.Message{WhatsAppMessageID: later.ID}).Count(&refusedRows).Error)
			require.Zero(t, refusedRows)
			followUp, _, admitErr = admitter.persistIncomingMessageBeforeAck(env.account.PhoneID, later, "H1 handover patient")
			require.NoError(t, admitErr)
			require.NotNil(t, followUp)
			require.Equal(t, env.work.Contact.ID, followUp.Contact.ID)
			// The production continuation of the follow-up.
			processor := NewInboundContinuationProcessor(env.app, time.Second)
			require.NoError(t, processor.ProcessMessage(context.Background(), env.org.ID, followUp.Persisted.ID))
			followUpJob := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, followUp.Persisted.ID)
			assert.Equal(t, models.ScheduledJobStatusCompleted, followUpJob.Status)
			assert.EqualValues(t, 1, env.calls.Load(), "exactly one transfer body")
			assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
			assert.Len(t, env.transfers(t), 1)

			holders.Wait()
			h1RequireLiveAfterRun(t, env)
		})
	}
}

// Overlapping in-flight AI attempts of other conversations (6s each, one every 2s,
// starting at the pre-handover boundary and outlasting the budget) keep the
// organization row KEY SHARE locked without a gap. The handover must give up within
// its 40s fence budget as a refunded deferral with nothing committed (it used to
// wait as long as the overlap lasted), and replay once the attempts stop.
func TestH1GraphTransferHandoverFenceBudgetUnderOverlappingAIAttempts(t *testing.T) {
	h1RequireSlow(t)
	env := h1HandoverFixture(t, h1HandoverOptions{flowCfg: map[string]any{"body": "Connecting you to a human."}})

	var holders, generator sync.WaitGroup
	var stop atomic.Bool
	var started atomic.Int32
	var boundaryAt time.Time
	ran := env.h1AfterOuterAdvisoryCount(t, "h1:overlapping_ai_attempts", func() {
		boundaryAt = time.Now()
		first := make(chan struct{})
		generator.Add(1)
		go func() {
			defer generator.Done()
			signalled := false
			for !stop.Load() && time.Since(boundaryAt) < 75*time.Second {
				acquired := make(chan error, 1)
				h1HoldAIAttemptKeyShare(env, 6*time.Second, acquired, &holders)
				if err := <-acquired; err == nil {
					started.Add(1)
				}
				if !signalled {
					signalled = true
					close(first)
				}
				time.Sleep(2 * time.Second)
			}
		}()
		<-first // the handover starts only once an attempt is in flight
	})

	elapsed, firstErr := h1RunWithWatchdog(t, env, env.baseTime, 120*time.Second)
	sinceBoundary := time.Since(boundaryAt)
	stopped := !stop.Swap(true)
	generator.Wait()
	holders.Wait()
	require.True(t, ran.Load())
	require.True(t, stopped)
	t.Logf("H1_HANDOVER_FENCE_BUDGET run=%s since_boundary=%s attempts_started=%d err=%v", elapsed, sinceBoundary, started.Load(), firstErr)
	h1RequireFenceBudgetDeferral(t, firstErr)
	assert.GreaterOrEqual(t, sinceBoundary, 39*time.Second, "the handover used its fence budget")
	assert.Less(t, sinceBoundary, 50*time.Second, "the handover gave up within its 40s fence budget")
	assert.GreaterOrEqual(t, started.Load(), int32(15), "the attempts overlapped for the whole budget")

	first := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
	assert.Equal(t, 0, first.Job.Attempts)
	assert.Equal(t, 1, h1JobDeferrals(first.Job))
	assert.False(t, first.InboundCompleted)
	assert.Zero(t, first.ActiveTransfers)
	assert.Equal(t, models.SessionStatusActive, first.Session.Status)
	assert.Equal(t, 0, first.Ordinal)
	assert.False(t, first.ContactCutoff)
	assert.EqualValues(t, 1, first.ProviderCalls)
	assert.Len(t, first.Outgoing, 1)

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

// Other continuations of the same round-robin team (each pinning its own pooled
// connection, maxOpen-1 of them) run the same member UPDATE right after this
// handover's selection. The selection must already be committed, so they pass and
// release their connections, and the handover gets its own connection without a
// 15s wedge-guard step. On the continuation transaction the row lock was held
// across that wait: the peers queued behind it and the handover deferred.
func TestH1GraphTransferTeamSelectionCommitsBeforeHandoverWait(t *testing.T) {
	const maxOpen = 3 // tx A plus the peers fill the pool at the handover boundary
	env := h1HandoverFixture(t, h1HandoverOptions{
		route:   "team",
		flowCfg: map[string]any{"body": "Connecting you to a human."},
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
	env.sqlPool.SetMaxOpenConns(maxOpen)

	var hooked, selectedOnOuter, poolFilled atomic.Bool
	var peers sync.WaitGroup
	var peerMu sync.Mutex
	var peerErrors []error
	const name = "h1:team_selection_peers"
	require.NoError(t, env.pool.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "team_members" || !hooked.CompareAndSwap(false, true) {
			return
		}
		if execution := env.execution.Load(); execution != nil && tx.Statement.ConnPool == execution.DB.Statement.ConnPool {
			selectedOnOuter.Store(true)
		}
		// Each peer waits for its own pooled connection; this callback only waits until
		// the pool is full (on the continuation transaction both peers hold one and block
		// on the member row; in a selection transaction of its own, one peer holds the
		// last connection and the other waits for the selection's).
		defer func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if env.sqlPool.Stats().InUse >= maxOpen {
					poolFilled.Store(true)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		for i := 0; i < maxOpen-1; i++ {
			peers.Add(1)
			go func() {
				defer peers.Done()
				err := func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					conn, err := env.sqlPool.Conn(ctx)
					if err != nil {
						return err
					}
					defer conn.Close()
					peerTx, err := conn.BeginTx(ctx, &sql.TxOptions{})
					if err != nil {
						return err
					}
					defer func() { _ = peerTx.Rollback() }()
					if _, err := peerTx.ExecContext(ctx,
						"UPDATE team_members SET last_assigned_at = now() WHERE team_id = $1 AND user_id = $2",
						env.team.ID, member.ID); err != nil {
						return err
					}
					time.Sleep(500 * time.Millisecond) // the rest of that continuation's work
					return peerTx.Commit()
				}()
				peerMu.Lock()
				peerErrors = append(peerErrors, err)
				peerMu.Unlock()
			}()
		}
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Update().Remove(name) })

	elapsed, runErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
	peersDone := make(chan struct{})
	go func() { peers.Wait(); close(peersDone) }()
	select {
	case <-peersDone:
	case <-time.After(60 * time.Second):
		t.Fatalf("the same-team peers never finished\n%s", h1PoolSessionReport(env.shared))
	}
	require.True(t, hooked.Load(), "the round-robin selection ran")
	require.True(t, poolFilled.Load(), "the same-team peers filled the pool at the selection")
	t.Logf("H1_TEAM_SELECTION run=%s err=%v", elapsed, runErr)
	assert.False(t, selectedOnOuter.Load(), "the round-robin UPDATE must not run on the continuation transaction")
	require.NoError(t, runErr, "the handover must not wait out its pool budget behind same-team peers")
	assert.Less(t, elapsed, 10*time.Second, "no 15s wedge-guard step")
	peerMu.Lock()
	for _, err := range peerErrors {
		assert.NoError(t, err)
	}
	assert.Len(t, peerErrors, maxOpen-1)
	peerMu.Unlock()

	state := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
	assert.Zero(t, h1JobDeferrals(state.Job))
	assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
	assert.EqualValues(t, 1, state.ProviderCalls)
	transfers := env.transfers(t)
	require.Len(t, transfers, 1)
	require.NotNil(t, transfers[0].AgentID)
	assert.Equal(t, member.ID, *transfers[0].AgentID)
	require.NotNil(t, transfers[0].TeamID)
	assert.Equal(t, env.team.ID, *transfers[0].TeamID)
	env.sqlPool.SetMaxOpenConns(10)
	h1RequireLiveAfterRun(t, env)
}

// The advisory and organization-row waits share one deadline. A per-statement
// timeout alone would spend 20s on the first lock and another 40s on the second.
func TestH1HandoverFenceDeadlineCoversBothStatements(t *testing.T) {
	h1RequireSlow(t)
	env := h1HandoverFixture(t, h1HandoverOptions{flowCfg: map[string]any{"body": "Connecting you to a human."}})
	releaseAI := make(chan struct{})
	var holders sync.WaitGroup
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(releaseAI) }); holders.Wait() }()
	var holderErrors [2]error
	var started time.Time
	ran := env.h1AfterOuterAdvisoryCount(t, "h1:two_statement_fence_deadline", func() {
		for i := 0; i < 2; i++ {
			ready := make(chan error, 1)
			holders.Add(1)
			go func(index int) {
				defer holders.Done()
				tx := env.shared.Begin()
				if tx.Error != nil {
					ready <- tx.Error
					return
				}
				defer tx.Rollback()
				if index == 0 {
					key := database.WhatsAppIdentityReviewContactSelectorFenceKey(env.org.ID)
					if err := tx.Exec("SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))", key).Error; err != nil {
						ready <- err
						return
					}
				} else if err := database.LockOrganizationAIAttemptScope(tx, env.org.ID); err != nil {
					ready <- err
					return
				}
				ready <- nil
				if index == 0 {
					time.Sleep(20 * time.Second)
				} else {
					select {
					case <-releaseAI:
					case <-time.After(100 * time.Second):
					}
				}
			}(i)
			holderErrors[i] = <-ready
		}
		started = time.Now()
	})
	_, firstErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
	elapsed := time.Since(started)
	releaseOnce.Do(func() { close(releaseAI) })
	holders.Wait()
	require.True(t, ran.Load())
	for _, err := range holderErrors {
		require.NoError(t, err)
	}
	h1RequireFenceBudgetDeferral(t, firstErr)
	assert.GreaterOrEqual(t, elapsed, 39*time.Second)
	assert.Less(t, elapsed, 50*time.Second, "the first wait must consume the second statement's budget")
	state := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusPending, state.Job.Status)
	assert.Zero(t, state.Job.Attempts)
	assert.Equal(t, 1, h1JobDeferrals(state.Job))
	assert.Zero(t, state.ActiveTransfers)
	assert.False(t, state.InboundCompleted)
	require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
	replayed := env.state(t)
	assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
	assert.EqualValues(t, 1, replayed.ActiveTransfers)
	assert.EqualValues(t, 1, replayed.ProviderCalls)
	h1RequireLiveAfterRun(t, env)
}
