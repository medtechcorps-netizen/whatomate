package handlers

// Isolated review tests for the keyword outcome proposal on H1 953a2654.
// The hours-transition diagnostic is deliberately separate from the P2 cohort:
// it asserts the desired reply and is expected to expose an unfixed ledger issue.
// Every database callback belongs to this fixture's private pool, never the shared
// migration pool. All messages and provider responses are synthetic.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

const h1KeywordTransferBody = "Synthetic keyword: connecting you to an agent"
const h1KeywordClosedBody = "Synthetic keyword: the office is closed"

type h1KeywordOutcomeEnv struct {
	app       *App
	shared    *gorm.DB
	pool      *gorm.DB
	sqlPool   *sql.DB
	org       *models.Organization
	account   *models.WhatsAppAccount
	settings  models.ChatbotSettings
	work      *persistedIncomingMessage
	processor *InboundContinuationProcessor
	baseTime  time.Time
	execution atomic.Pointer[App]
	mu        sync.Mutex
	sent      []string
}

func h1KeywordOutcomeFixture(t *testing.T) *h1KeywordOutcomeEnv {
	t.Helper()
	app := newProcessorTestApp(t)
	require.NotNil(t, app.Redis, "this cohort requires the coordinated disposable Redis fixture")
	shared := app.DB
	org, account := createProcessorTestOrg(t, app)
	settings := models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		WhatsAppAccount: account.Name, IsEnabled: true, SessionTimeoutMins: 30,
		AI: models.AIConfig{Enabled: false},
	}
	require.NoError(t, shared.Create(&settings).Error)
	require.NoError(t, shared.Create(&models.KeywordRule{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		WhatsAppAccount: account.Name, Name: "h1-keyword-outcome", Keywords: models.StringArray{"agent"},
		MatchType: models.MatchTypeExact, ResponseType: models.ResponseTypeTransfer,
		ResponseContent: models.JSONB{"body": h1KeywordTransferBody}, Priority: 10, IsEnabled: true,
	}).Error)
	message := inboundContinuationTextMessage(t, "wamid.h1-keyword-outcome-"+uuid.NewString(),
		"6056"+uuid.NewString()[:8], "agent")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic keyword patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	pool, sqlPool := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	env := &h1KeywordOutcomeEnv{app: app, shared: shared, pool: pool, sqlPool: sqlPool,
		org: org, account: account, settings: settings, work: work, baseTime: time.Now().UTC()}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		env.mu.Lock()
		env.sent = append(env.sent, request.Text.Body)
		ordinal := len(env.sent)
		env.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.h1-keyword.%s.%d", org.ID, ordinal)}},
		})
	}))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	env.processor = NewInboundContinuationProcessor(app, time.Second)
	env.processor.process = func(ctx context.Context, scoped *App, work *persistedIncomingMessage) error {
		env.execution.Store(scoped)
		return scoped.continuePersistedIncomingMessage(ctx, work)
	}
	return env
}

func (env *h1KeywordOutcomeEnv) bodies() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	return append([]string(nil), env.sent...)
}

func (env *h1KeywordOutcomeEnv) run(at time.Time) error {
	env.processor.now = func() time.Time { return at }
	return env.processor.ProcessMessage(context.Background(), env.org.ID, env.work.Persisted.ID)
}

// Arm only the helper's checked COUNT after the transfer body has returned and
// released its physical-attempt guard. Ignore the earlier transfer check and the
// provider guard's policy reads, which run on other connections.
func (env *h1KeywordOutcomeEnv) beforeHelperCount(t *testing.T, action func(*gorm.DB)) *atomic.Bool {
	t.Helper()
	var fired atomic.Bool
	name := "h1:keyword_checked_count:" + uuid.NewString()
	require.NoError(t, env.pool.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		execution := env.execution.Load()
		if execution == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "agent_transfers" ||
			tx.Statement.ConnPool != execution.DB.Statement.ConnPool || len(env.bodies()) != 1 {
			return
		}
		if _, count := tx.Statement.Dest.(*int64); !count || !fired.CompareAndSwap(false, true) {
			return
		}
		require.NotNil(t, execution.inboundContinuation)
		require.False(t, execution.inboundContinuation.attemptGuarded)
		require.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
		action(tx)
	}))
	t.Cleanup(func() { _ = env.pool.Callback().Query().Remove(name) })
	return &fired
}

func (env *h1KeywordOutcomeEnv) closeHours(t *testing.T, reply string) {
	t.Helper()
	require.NoError(t, env.shared.Model(&models.ChatbotSettings{}).
		Where("id = ? AND organization_id = ?", env.settings.ID, env.org.ID).
		Updates(map[string]any{"business_hours_enabled": true, "business_hours": h1AllDaysClosed(),
			"out_of_hours_message": reply, "allow_automated_outside_hours": false}).Error)
	key := settingsCachePrefix + env.org.ID.String() + ":" + env.account.Name
	require.NoError(t, env.app.Redis.Del(context.Background(), key).Err())
}

func (env *h1KeywordOutcomeEnv) job(t *testing.T) models.ScheduledJob {
	t.Helper()
	return loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
}

func TestH1KeywordOutcome_CheckedLookupErrorCountsAndReplaysBodyOnce(t *testing.T) {
	env := h1KeywordOutcomeFixture(t)
	lookupErr := errors.New("synthetic checked keyword lookup failure")
	fired := env.beforeHelperCount(t, func(tx *gorm.DB) { _ = tx.AddError(lookupErr) })
	err := env.run(env.baseTime)
	require.True(t, fired.Load())
	require.ErrorIs(t, err, lookupErr)
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "wedge guard")
	first := env.job(t)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Status)
	assert.Equal(t, 1, first.Attempts)
	_, refunded := first.Payload["wedge_guard_deferrals"]
	assert.False(t, refunded)
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
}

func TestH1KeywordOutcome_SecondCheckIntentionalDecline(t *testing.T) {
	for name, reply := range map[string]string{"without_reply": "", "with_reply": h1KeywordClosedBody} {
		t.Run(name, func(t *testing.T) {
			env := h1KeywordOutcomeFixture(t)
			fired := env.beforeHelperCount(t, func(_ *gorm.DB) { env.closeHours(t, reply) })
			require.NoError(t, env.run(env.baseTime))
			require.True(t, fired.Load(), "hours must change after the first check and accepted transfer body")
			job := env.job(t)
			assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
			assert.Equal(t, 1, job.Attempts)
			_, refunded := job.Payload["wedge_guard_deferrals"]
			assert.False(t, refunded)
			assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
			want := []string{h1KeywordTransferBody}
			if reply != "" {
				want = append(want, reply)
			}
			assert.Equal(t, want, env.bodies())
			require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
			assert.Equal(t, want, env.bodies(), "settled inbound must not resend either body")
		})
	}
}

// Reuse the existing H1 probe's safe release/snapshot, but match the genuine
// keyword error verbatim. No synthetic error or rewritten production log.
type h1KeywordWaitProbe struct{ h1HandoverWaitProbe }

func (p *h1KeywordWaitProbe) Write(data []byte) (int, error) {
	p.mu.Lock()
	_, _ = p.logs.Write(data)
	line := string(data)
	matched := p.armed && strings.Contains(line, "Failed to create keyword-triggered transfer") &&
		strings.Contains(line, "wedge guard: no pooled database connection within 15s") &&
		strings.Contains(line, "context deadline exceeded")
	if matched && !p.timeoutObserved {
		p.timeoutObserved, p.timeoutAt, p.matchingLog = true, time.Now(), strings.TrimSpace(line)
	}
	p.mu.Unlock()
	if matched {
		p.release("actual-keyword-timeout-log")
	}
	return len(data), nil
}

func TestH1KeywordOutcome_RealPoolWaitRefundsAndReplaysBodyOnce(t *testing.T) {
	h1RequireSlow(t)
	env := h1KeywordOutcomeFixture(t)
	probe := &h1KeywordWaitProbe{h1HandoverWaitProbe: h1HandoverWaitProbe{pool: env.sqlPool}}
	env.app.Log = logf.New(logf.Opts{Writer: probe, Level: logf.ErrorLevel})
	t.Cleanup(func() { probe.release("test-cleanup") })
	started := time.Now()
	fired := env.beforeHelperCount(t, func(tx *gorm.DB) {
		execution := env.execution.Load()
		rootPool, ok := execution.rootApp().DB.Statement.ConnPool.(*sql.DB)
		require.True(t, ok)
		require.Same(t, env.sqlPool, rootPool)
		require.Same(t, execution.DB.Statement.ConnPool, tx.Statement.ConnPool)
		require.NoError(t, env.pool.Statement.Context.Err())
		require.Less(t, time.Since(started), 10*time.Second, "the 30s heartbeat must not compete during the 15s barrier")
		require.Equal(t, 1, env.sqlPool.Stats().InUse)
		require.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
		env.sqlPool.SetMaxOpenConns(2)
		env.sqlPool.SetMaxIdleConns(0)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		held, err := env.sqlPool.Conn(ctx)
		cancel()
		require.NoError(t, err)
		probe.mu.Lock()
		probe.held, probe.armed, probe.blockedAt = held, true, time.Now()
		probe.timer = time.AfterFunc(45*time.Second, func() { probe.release("watchdog-no-keyword-timeout") })
		probe.mu.Unlock()
		stats := env.sqlPool.Stats()
		require.Equal(t, 2, stats.OpenConnections)
		require.Equal(t, 2, stats.InUse)
		require.Zero(t, stats.Idle)
		require.Equal(t, 2, stats.MaxOpenConnections)
	})
	err := env.run(env.baseTime)
	probe.release("process-returned-without-timeout-log")
	require.True(t, fired.Load())
	h1RequireWedgeGuard(t, err)
	first := env.job(t)
	assert.Equal(t, models.ScheduledJobStatusPending, first.Status)
	assert.Zero(t, first.Attempts)
	deferrals, ok := inboundContinuationExactInteger(first.Payload["wedge_guard_deferrals"])
	require.True(t, ok)
	assert.Equal(t, 1, deferrals)
	assert.Zero(t, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
	wait := probe.snapshot()
	raw, marshalErr := json.Marshal(wait)
	require.NoError(t, marshalErr)
	t.Logf("H1_KEYWORD_REAL_WAIT %s", raw)
	require.Equal(t, true, wait["actual_timeout_observed"])
	require.Equal(t, "actual-keyword-timeout-log", wait["release_reason"])
	require.Empty(t, wait["release_error"])
	require.GreaterOrEqual(t, wait["blocked_to_timeout_ms"].(int64), int64(15000))
	require.NoError(t, env.run(env.baseTime.Add(2*time.Second)))
	assert.Equal(t, models.ScheduledJobStatusCompleted, env.job(t).Status)
	assert.Equal(t, []string{h1KeywordTransferBody}, env.bodies())
	assert.Len(t, h1OutgoingMessages(t, env.shared, env.org.ID, env.work.Contact.ID), 1)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, env.shared, env.org.ID, env.work.Contact.ID))
}
