package handlers

// H1 regressions from the adversarial liveness review of f4f89cf9: the team route's
// own agent-selection transaction (createTransferToTeam) had no lock bound. Inside a
// continuation it pins tx A plus its own connection, and round-robin UPDATEs the
// member with the oldest last_assigned_at, which every concurrent same-team selection
// picks. An HTTP CreateAgentTransfer to the same team holds that row on its request
// transaction across its own unbounded organization-fence wait, so the graph handover
// waited as long as that request did. Every lock wait of the selection is now bounded
// at 3s: a real lock timeout is returned with its SQL cause and becomes a refunded
// deferral. Failed selection never supplies an agent to the handover.
//
// Every case drives real ProcessMessage settlement on a dedicated pool (h1OpenPool);
// callbacks are registered only on that pool. None of these tests calls t.Parallel.

import (
	"context"
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
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

// h1LogLineProbe closes release the first time a log line contains every needle.
type h1LogLineProbe struct {
	needles  []string
	release  chan struct{}
	released sync.Once
	mu       sync.Mutex
	matched  string
	at       time.Time
}

// Ordinary selection failures must consume an attempt; an aborted-transaction
// error without its original cause is not evidence of congestion. Inject at the
// real checked member-read boundary, then replay after removing the failure.
func TestH1TeamSelectionOrdinarySQLFailureConsumesAttempt(t *testing.T) {
	for _, code := range []string{"42501", "25P02", "57014"} {
		t.Run(code, func(t *testing.T) {
			env, member := h1TeamSelectionFixture(t)
			cacheKey := "team:assignment:" + env.team.ID.String()
			require.NoError(t, env.app.Redis.Del(context.Background(), cacheKey).Err())
			failure := &pgconn.PgError{Code: code, Message: "synthetic ordinary team selection failure"}
			var hits atomic.Int32
			const name = "h1:team_selection_ordinary_error"
			require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Table == "team_members" {
					hits.Add(1)
					_ = tx.AddError(failure)
				}
			}))
			t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
			err := env.run(t, env.baseTime)
			require.ErrorIs(t, err, failure)
			assert.Equal(t, code, postgresErrorCode(err))
			assert.NotContains(t, err.Error(), "wedge guard")
			assert.EqualValues(t, 1, hits.Load())
			first := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
			assert.Equal(t, 1, first.Job.Attempts)
			assert.Zero(t, h1JobDeferrals(first.Job))
			assert.Zero(t, first.ActiveTransfers)
			assert.False(t, first.InboundCompleted)
			assert.False(t, first.ContactCutoff)
			assert.Equal(t, models.SessionStatusActive, first.Session.Status)
			assert.EqualValues(t, 1, first.ProviderCalls)
			cached, cacheErr := env.app.Redis.Exists(context.Background(), cacheKey).Result()
			require.NoError(t, cacheErr)
			assert.Zero(t, cached)
			require.NoError(t, env.pool.Callback().Query().Remove(name))
			require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
			replayed := env.state(t)
			assert.Equal(t, models.ScheduledJobStatusCompleted, replayed.Job.Status)
			assert.True(t, replayed.InboundCompleted)
			assert.EqualValues(t, 1, replayed.ProviderCalls, "recovery reuses the accepted body")
			transfers := env.transfers(t)
			require.Len(t, transfers, 1)
			require.NotNil(t, transfers[0].AgentID)
			assert.Equal(t, member.ID, *transfers[0].AgentID)
		})
	}
}

func (p *h1LogLineProbe) Write(data []byte) (int, error) {
	line := string(data)
	for _, needle := range p.needles {
		if !strings.Contains(line, needle) {
			return len(data), nil
		}
	}
	p.mu.Lock()
	if p.matched == "" {
		p.matched = strings.TrimSpace(line)
		p.at = time.Now()
	}
	p.mu.Unlock()
	p.released.Do(func() { close(p.release) })
	return len(data), nil
}

func (p *h1LogLineProbe) observed() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.matched, p.at
}

// h1TeamSelectionFixture is a team-route handover with one available agent member and
// the production round-robin Assigner on the fixture's dedicated pool.
func h1TeamSelectionFixture(t *testing.T) (*h1HandoverEnv, *models.User) {
	t.Helper()
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
	return env, member
}

func TestH1GraphTransferTeamSelectionLockWaitIsBounded(t *testing.T) {
	// The realistic holder: a real HTTP CreateAgentTransfer (through App.Tenant) to the
	// same team selects the same member on its request transaction and then waits on the
	// organization fence, held here by another fence holder that is released only once
	// the graph's selection gives up its row wait (or by a 45s watchdog). Unbounded, the
	// graph run lasted as long as that fence holder while pinning tx A and the selection
	// connection. The bounded selection now defers with its real error after 3s;
	// recovery commits once the fence frees, without repeating the accepted body.
	t.Run("http_transfer_holds_member_row_across_fence_wait", func(t *testing.T) {
		env, member := h1TeamSelectionFixture(t)
		adminRole := testutil.CreateAdminRole(t, env.shared, env.org.ID)
		admin := testutil.CreateTestUser(t, env.shared, env.org.ID, testutil.WithRoleID(&adminRole.ID))
		other := testutil.CreateTestContact(t, env.shared, env.org.ID)
		req := testutil.NewJSONRequest(t, map[string]any{
			"contact_id": other.ID.String(), "whatsapp_account": env.account.Name,
			"team_id": env.team.ID.String(), "source": models.TransferSourceManual,
		})
		testutil.SetAuthContext(req, env.org.ID, admin.ID)
		probe := &h1LogLineProbe{
			needles: []string{"Failed to select a team agent for the transfer"},
			release: make(chan struct{}),
		}
		env.app.Log = logf.New(logf.Opts{Writer: probe, Level: logf.WarnLevel})
		key := database.WhatsAppIdentityReviewContactSelectorFenceKey(env.org.ID)

		var boundaryAt time.Time
		var fenceErr error
		var fenceHeldFor time.Duration
		var httpWaitingOnFence atomic.Bool
		fenceDone := make(chan struct{})
		httpDone := make(chan error, 1)
		ran := env.h1AfterOuterAdvisoryCount(t, "h1:team_selection_http_holder", func() {
			boundaryAt = time.Now()
			held := make(chan error, 1)
			go func() {
				defer close(fenceDone)
				holder := env.shared.Begin()
				if holder.Error != nil {
					held <- holder.Error
					return
				}
				defer holder.Rollback()
				if err := holder.Exec("SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))", key).Error; err != nil {
					held <- err
					return
				}
				held <- nil
				lockedAt := time.Now()
				select {
				case <-probe.release:
				case <-time.After(45 * time.Second): // watchdog: the selection never gave up
				}
				fenceHeldFor = time.Since(lockedAt)
			}()
			if fenceErr = <-held; fenceErr != nil {
				return
			}
			go func() {
				httpDone <- env.app.Tenant(func(scoped *App, r *fastglue.Request) error {
					return scoped.CreateAgentTransfer(r)
				})(req)
			}()
			// The handover starts only once the request holds the member row and waits on
			// the organization fence.
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				var waiting int64
				_ = env.shared.Raw(`SELECT count(*) FROM pg_stat_activity WHERE application_name = 'h1_pool_bound_test'
					AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting).Error
				if waiting > 0 {
					httpWaitingOnFence.Store(true)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		})

		elapsed, runErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
		require.True(t, ran.Load(), "the pre-handover boundary was reached")
		select {
		case <-fenceDone:
		case <-time.After(60 * time.Second):
			t.Fatal("the outside fence holder never finished")
		}
		var httpErr error
		select {
		case httpErr = <-httpDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("the HTTP transfer never returned\n%s", h1PoolSessionReport(env.shared))
		}
		require.NoError(t, fenceErr, "the outside session must hold the organization fence")
		require.True(t, httpWaitingOnFence.Load(), "the HTTP transfer held the member row while waiting on the fence")
		matched, matchedAt := probe.observed()
		t.Logf("H1_TEAM_SELECTION_HTTP run=%s fence_held=%s err=%v log=%s", elapsed, fenceHeldFor, runErr, matched)
		require.NotEmpty(t, matched, "the graph selection must give up its member-row wait (fence held %s)", fenceHeldFor)
		gaveUpAfter := matchedAt.Sub(boundaryAt)
		assert.GreaterOrEqual(t, gaveUpAfter, 2500*time.Millisecond, "the selection waited for the row up to its bound")
		assert.Less(t, gaveUpAfter, 10*time.Second, "the selection's row wait is bounded at 3s")
		assert.Less(t, fenceHeldFor, 15*time.Second)

		h1RequireWedgeGuard(t, runErr)
		assert.Equal(t, "55P03", postgresErrorCode(runErr), "preserve the actual selection lock timeout")
		first := env.state(t)
		assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
		assert.Zero(t, first.Job.Attempts)
		assert.Equal(t, 1, h1JobDeferrals(first.Job))
		assert.Zero(t, first.ActiveTransfers)
		assert.EqualValues(t, 1, first.ProviderCalls)
		require.NoError(t, env.run(t, env.baseTime.Add(2*time.Second)))
		assert.Less(t, elapsed, 20*time.Second)
		require.NoError(t, httpErr)
		assert.Equal(t, fasthttp.StatusOK, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))

		state := env.state(t)
		assert.Equal(t, models.ScheduledJobStatusCompleted, state.Job.Status)
		assert.Equal(t, 1, h1JobDeferrals(state.Job))
		assert.True(t, state.InboundCompleted)
		assert.Equal(t, models.SessionStatusCompleted, state.Session.Status)
		assert.True(t, state.ContactCutoff)
		assert.EqualValues(t, 1, state.ProviderCalls)
		assert.Len(t, state.Outgoing, 1)
		transfers := env.transfers(t)
		require.Len(t, transfers, 1)
		require.NotNil(t, transfers[0].AgentID)
		assert.Equal(t, member.ID, *transfers[0].AgentID)
		require.NotNil(t, transfers[0].TeamID)
		assert.Equal(t, env.team.ID, *transfers[0].TeamID)

		var httpTransfer models.AgentTransfer
		require.NoError(t, env.shared.Where("organization_id = ? AND contact_id = ? AND status = ?",
			env.org.ID, other.ID, models.TransferStatusActive).First(&httpTransfer).Error)
		require.NotNil(t, httpTransfer.AgentID)
		assert.Equal(t, member.ID, *httpTransfer.AgentID, "the HTTP request selected the same member")
		h1RequireLiveAfterRun(t, env)
	})

	// A selection that is aborted before it selects anyone: a relation lock on
	// team_members (as DDL takes) held past the bound while the team configuration is
	// not cached. The checked Assigner preserves the member-read 55P03 and does not
	// cache an incomplete configuration. The refunded deferral commits no handover;
	// replay selects the member and hands over once without resending the body.
	t.Run("aborted_selection_without_agent_is_refunded_deferral", func(t *testing.T) {
		env, member := h1TeamSelectionFixture(t)
		cacheKey := "team:assignment:" + env.team.ID.String()
		require.NoError(t, env.app.Redis.Del(context.Background(), cacheKey).Err())

		release := make(chan struct{})
		holderDone := make(chan struct{})
		var holderErr error
		ran := env.h1AfterOuterAdvisoryCount(t, "h1:team_members_relation_lock", func() {
			ready := make(chan error, 1)
			go func() {
				defer close(holderDone)
				holder := env.shared.Begin()
				if holder.Error != nil {
					ready <- holder.Error
					return
				}
				defer holder.Rollback()
				if err := holder.Exec("SET LOCAL lock_timeout = '10s'").Error; err != nil {
					ready <- err
					return
				}
				if err := holder.Exec("LOCK TABLE team_members IN ACCESS EXCLUSIVE MODE").Error; err != nil {
					ready <- err
					return
				}
				ready <- nil
				select {
				case <-release:
				case <-time.After(60 * time.Second): // watchdog: the selection never gave up
				}
			}()
			holderErr = <-ready
		})

		elapsed, firstErr := h1RunWithWatchdog(t, env, env.baseTime, 90*time.Second)
		close(release)
		select {
		case <-holderDone:
		case <-time.After(70 * time.Second):
			t.Fatal("the outside relation lock was never released")
		}
		require.True(t, ran.Load(), "the pre-handover boundary was reached")
		require.NoError(t, holderErr, "the outside session must hold team_members")
		t.Logf("H1_TEAM_SELECTION_ABORTED run=%s err=%v", elapsed, firstErr)

		h1RequireWedgeGuard(t, firstErr)
		assert.Equal(t, "55P03", postgresErrorCode(firstErr), "the original member-read timeout stays inspectable")
		assert.False(t, inboundContinuationRequiresManualReview(firstErr))
		assert.False(t, inboundContinuationStoppedByPolicy(firstErr))
		assert.GreaterOrEqual(t, elapsed, 2500*time.Millisecond, "the member read waited up to its bound")
		assert.Less(t, elapsed, 20*time.Second, "the selection's lock wait is bounded at 3s")
		cached, err := env.app.Redis.Exists(context.Background(), cacheKey).Result()
		require.NoError(t, err)
		assert.Zero(t, cached, "a failed member read never caches an incomplete team configuration")

		first := env.state(t)
		assert.Equal(t, models.ScheduledJobStatusPending, first.Job.Status)
		assert.Equal(t, 0, first.Job.Attempts, "an aborted selection is a refunded deferral")
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
		assert.Equal(t, models.SessionStatusCompleted, replayed.Session.Status)
		assert.Equal(t, 1, replayed.T1Entries)
		assert.EqualValues(t, 1, replayed.ProviderCalls, "recovery must not resend the body")
		assert.Len(t, replayed.Outgoing, 1)
		transfers := env.transfers(t)
		require.Len(t, transfers, 1)
		require.NotNil(t, transfers[0].AgentID, "the replay reloads the team configuration and selects the member")
		assert.Equal(t, member.ID, *transfers[0].AgentID)
		require.NotNil(t, transfers[0].TeamID)
		assert.Equal(t, env.team.ID, *transfers[0].TeamID)
		h1RequireLiveAfterRun(t, env)
	})
}
