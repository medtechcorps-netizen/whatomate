package handlers

// Ingress must relinquish a connection while a physical AI attempt owns the
// organization KEY SHARE guard. These tests use real PostgreSQL locks, the
// signed webhook admission path, and the real durable continuation processor.
// They do not contact Meta or an AI provider, or weaken tenant/policy checks.
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1IngressResolver(t *testing.T, shared *gorm.DB) {
	t.Helper()
	// Same synthetic RLS resolver used by the sustained fixture; the production
	// admission still resolves authority and sets the real tenant transaction.
	require.NoError(t, shared.Exec(`CREATE OR REPLACE FUNCTION public.rereply_resolve_whatsapp_org(p_phone_id text)
		RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $function$
		SELECT organization_id FROM public.whatsapp_accounts
		WHERE BTRIM(phone_id)=BTRIM(p_phone_id) AND deleted_at IS NULL $function$`).Error)
}

func h1IngressFixture(t *testing.T, smb bool) (*App, *gorm.DB, *sql.DB, models.WhatsAppAccount) {
	t.Helper()
	app := webhookTestApp(t)
	shared := app.DB
	account := createCoexistenceAdmissionWebhookAccount(t, app)
	if !smb {
		require.NoError(t, shared.Model(&account).Update("is_smb", false).Error)
		account.IsSMB = false
	}
	pool, sqlPool := h1OpenPool(t, 10)
	app.DB = pool
	app.Config.Database.RLSEnabled = true
	h1IngressResolver(t, shared)
	return app, shared, sqlPool, account
}

func h1IngressHoldAIGuard(t *testing.T, shared *gorm.DB, org uuid.UUID) func() {
	t.Helper()
	tx := shared.Begin()
	require.NoError(t, tx.Error)
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback().Error }) }
	t.Cleanup(release)
	require.NoError(t, database.LockOrganizationAIAttemptScope(tx, org))
	return release
}

func h1IngressWaiters(t *testing.T, shared *gorm.DB, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var count int64
		err := shared.Raw(`SELECT count(*) FROM pg_stat_activity
			WHERE application_name='h1_pool_bound_test' AND wait_event_type='Lock'
			AND (query LIKE '%FOR UPDATE%' OR query LIKE '%pg_advisory_xact_lock%')`).Scan(&count).Error
		return err == nil && count == int64(want)
	}, 8*time.Second, 25*time.Millisecond, "the intended real organization fence must be waiting")
}

type h1IngressHTTPResult struct {
	status int
	err    error
}

func h1IngressSignedAsync(t *testing.T, app *App, body []byte, beforeJoin ...func()) <-chan h1IngressHTTPResult {
	t.Helper()
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", webhookTestSignature(body))
	req.RequestCtx.Request.SetBody(body)
	done := make(chan h1IngressHTTPResult, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		err := app.WebhookHandler(req)
		done <- h1IngressHTTPResult{req.RequestCtx.Response.StatusCode(), err}
	}()
	t.Cleanup(func() {
		for _, release := range beforeJoin {
			release()
		}
		select {
		case <-joined:
			h1IngressJoin(t, app)
		case <-time.After(35 * time.Second):
			t.Error("signed webhook worker did not join during cleanup")
		}
	})
	return done
}

func h1IngressJoin(t *testing.T, app *App) {
	t.Helper()
	done := make(chan struct{})
	go func() { app.WaitForBackgroundTasks(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("webhook continuations did not join")
	}
}

func h1IngressCounts(t *testing.T, shared *gorm.DB, org uuid.UUID) map[string]int64 {
	t.Helper()
	counts := map[string]int64{}
	for name, model := range map[string]any{
		"contacts": &models.Contact{}, "messages": &models.Message{},
		"jobs": &models.ScheduledJob{}, "outbox": &models.OutboxEvent{},
		"reviews": &models.InboundEvent{}, "holds": &models.WhatsAppIdentityReviewHold{},
		"activity": &models.CustomerActivityEvent{},
	} {
		var count int64
		require.NoError(t, shared.Unscoped().Model(model).Where("organization_id = ?", org).Count(&count).Error)
		counts[name] = count
	}
	return counts
}

func TestH1IngressFence_AuthenticatedTimeoutRollsBackAndReplays(t *testing.T) {
	for _, route := range []string{"classic", "coexistence"} {
		t.Run(route, func(t *testing.T) {
			app, shared, pool, account := h1IngressFixture(t, route == "coexistence")
			body := coexistenceAdmissionInboundBody(account, "wamid.h1-ingress-"+uuid.NewString(),
				coexistenceAdmissionTestPhone(), "US."+testutil.NewTestGraphObjectID()[:20], "Synthetic ingress")
			before := h1IngressCounts(t, shared, account.OrganizationID)
			release := h1IngressHoldAIGuard(t, shared, account.OrganizationID)
			started := time.Now()
			done := h1IngressSignedAsync(t, app, body, release)
			h1IngressWaiters(t, shared, 1)
			var result h1IngressHTTPResult
			select {
			case result = <-done:
			case <-time.After(28 * time.Second):
				// Baseline must be allowed to unwind before reporting the failure.
				release()
				select {
				case <-done:
					h1IngressJoin(t, app)
				case <-time.After(15 * time.Second):
					t.Fatal("baseline admission did not unwind after the guard was released")
				}
				t.Fatal("INGRESS_FENCE_UNBOUNDED: signed admission held its connection beyond the 20s fence budget")
			}
			require.NoError(t, result.err)
			require.Equal(t, http.StatusServiceUnavailable, result.status, "no ACK before durable admission")
			assert.GreaterOrEqual(t, time.Since(started), 19*time.Second, "real production deadline was exercised")
			assert.Less(t, time.Since(started), 28*time.Second)
			assert.Equal(t, before, h1IngressCounts(t, shared, account.OrganizationID), "failed admission rolls back all tenant writes")
			require.Eventually(t, func() bool { return pool.Stats().InUse == 0 }, 3*time.Second, 20*time.Millisecond)
			release()
			require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body), "same WAMID retries successfully")
			h1IngressJoin(t, app)
			after := h1IngressCounts(t, shared, account.OrganizationID)
			assert.EqualValues(t, before["contacts"]+1, after["contacts"])
			assert.EqualValues(t, before["messages"]+1, after["messages"])
			assert.EqualValues(t, before["jobs"]+1, after["jobs"])
			require.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, body))
			h1IngressJoin(t, app)
			assert.Equal(t, after, h1IngressCounts(t, shared, account.OrganizationID), "duplicate WAMID cannot recreate any effect")
		})
	}
}

func TestH1IngressFence_EarlyReleaseDoesNotCancelOwningTransaction(t *testing.T) {
	app, shared, _, account := h1IngressFixture(t, true)
	var fired atomic.Bool
	name := "h1:ingress_after_fence:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" || !fired.CompareAndSwap(false, true) {
			return
		}
		// The successful lock's deadline/cancel must not become the owning
		// transaction context, even when later work outlives that deadline.
		time.Sleep(21 * time.Second)
		if err := tx.Statement.Context.Err(); err != nil {
			_ = tx.AddError(fmt.Errorf("owning transaction inherited fence cancellation: %w", err))
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Create().Remove(name) })
	release := h1IngressHoldAIGuard(t, shared, account.OrganizationID)
	body := coexistenceAdmissionInboundBody(account, "wamid.h1-early-"+uuid.NewString(),
		coexistenceAdmissionTestPhone(), "US."+testutil.NewTestGraphObjectID()[:20], "Synthetic early release")
	done := h1IngressSignedAsync(t, app, body, release)
	h1IngressWaiters(t, shared, 1)
	release()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status)
	case <-time.After(35 * time.Second):
		t.Fatal("early release did not leave a usable owning transaction")
	}
	require.True(t, fired.Load())
	h1IngressJoin(t, app)
	assert.EqualValues(t, 1, h1IngressCounts(t, shared, account.OrganizationID)["messages"])
}

func TestH1IngressFence_CapturedAIResultSurvivesPoolPressure(t *testing.T) {
	env := h1HandoverFixture(t, h1HandoverOptions{})
	h1IngressResolver(t, env.shared)
	entered, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	finishAI := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(finishAI)
	var computations atomic.Int32
	dispatchErrors := make(chan error, 1)
	env.processor.process = func(_ context.Context, execution *App, work *persistedIncomingMessage) (processErr error) {
		// ProcessMessage may successfully record a failed/manual-review job and
		// return nil. Retain the dispatch error independently of settlement.
		defer func() { dispatchErrors <- processErr }()
		result, err := execution.runInboundContinuationCapturedAction("h1-ingress-ai", func() models.JSONB {
			computations.Add(1)
			close(entered)
			<-finish
			return models.JSONB{"answer": "Synthetic captured answer"}
		})
		if err != nil {
			return err
		}
		return execution.sendAndSaveTextMessage(&work.Account, &work.Contact, result["answer"].(string))
	}
	aiDone := make(chan error, 1)
	var running sync.WaitGroup
	t.Cleanup(func() {
		finishAI()
		joined := make(chan struct{})
		go func() { running.Wait(); close(joined) }()
		select {
		case <-joined:
			h1IngressJoin(t, env.app)
		case <-time.After(65 * time.Second):
			t.Error("AI and ingress workers did not join during cleanup")
		}
	})
	running.Add(1)
	go func() { defer running.Done(); aiDone <- env.run(t, env.baseTime) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("real durable AI guard and claim were not established")
	}
	gate := &env.app.rootApp().continuationAdmission
	gate.mu.Lock()
	continuationWeight := gate.used
	gate.mu.Unlock()
	require.Equal(t, 3, continuationWeight, "the custom callback reserves its actual outer/guard/provider demand")
	type admissionResult struct {
		index     int
		work      *persistedIncomingMessage
		duplicate bool
		err       error
	}
	results := make(chan admissionResult, 8)
	messages := make([]IncomingTextMessage, 8)
	for i := range messages {
		messages[i] = inboundContinuationTextMessage(t, "wamid.h1-pressure-"+uuid.NewString(),
			coexistenceAdmissionTestPhone(), "Synthetic concurrent ingress")
		message := messages[i]
		index := i
		running.Add(1)
		go func() {
			defer running.Done()
			work, duplicate, err := env.app.persistAuthenticatedIncomingMessageBeforeAck(
				env.account.PhoneID, message, "Synthetic ingress", strings.Repeat("a", 64))
			results <- admissionResult{index, work, duplicate, err}
		}()
	}
	// All eight deliveries remain. Their discovery queries release SQL and
	// global credits before the same-organization writer queue. One ingress
	// can wait at the real SQL fence; the other seven own neither resource.
	// The custom callback retains its ordinary outer+guard shape.
	budget := env.sqlPool.Stats().MaxOpenConnections - inboundContinuationProgressAllowance
	granted := 1
	require.Greater(t, budget-continuationWeight, granted, "organization serialization must be exercised with spare global credits")
	require.Greater(t, granted, 0)
	require.Less(t, granted, len(messages), "the fixture must exercise both admitted and queued ingress")
	queued := len(messages) - granted
	h1IngressWaiters(t, env.shared, granted)
	require.Eventually(t, func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		if gate.used != continuationWeight+granted || len(gate.waiters) != queued {
			return false
		}
		for _, waiter := range gate.waiters {
			if waiter.organization != env.org.ID.String() || waiter.mode != inboundContinuationWriter {
				return false
			}
		}
		return true
	}, 3*time.Second, 10*time.Millisecond, "all eight ingresses must reach either the real DB fence or the pre-DB admission queue")
	require.Equal(t, granted+2, env.sqlPool.Stats().InUse, "queued ingress cannot retain a SQL connection; outer continuation and physical guard account for the other two")
	require.LessOrEqual(t, env.sqlPool.Stats().InUse, env.sqlPool.Stats().MaxOpenConnections-inboundContinuationProgressAllowance, "independent result persistence retains real pool headroom")
	finishedAt := time.Now()
	finishAI()
	var aiErr error
	select {
	case aiErr = <-aiDone:
	case <-time.After(65 * time.Second):
		t.Fatal("captured result/continuation did not settle within its bounded waits")
	}
	admitted := make([]bool, len(messages))
	refused := 0
	for range messages {
		select {
		case result := <-results:
			if result.err != nil {
				refused++
				assert.Nil(t, result.work, "failed ingress exposes no uncommitted work")
				assert.True(t, errors.Is(result.err, context.DeadlineExceeded) || strings.Contains(result.err.Error(), "lock incoming organization admission"), "only a known bounded pre-ACK admission refusal is expected")
			} else {
				// Once the AI guard releases, remaining in-budget admissions
				// may legitimately win the lock instead of timing out.
				assert.NotNil(t, result.work)
				admitted[result.index] = true
			}
			assert.False(t, result.duplicate)
		case <-time.After(15 * time.Second):
			t.Fatal("ingress did not unwind after the physical guard ended")
		}
	}
	require.NoError(t, aiErr, "durable continuation settlement must succeed")
	require.NoError(t, <-dispatchErrors, "CAPTURED_RESULT_STARVED: a computed AI result must not become manual review behind ingress fences")
	// Admission headroom can now persist the captured result without waiting for
	// an ingress timeout. The separate authenticated-timeout regression still
	// proves rollback and redelivery for a real 20s fence expiry.
	t.Logf("eight ingress outcomes: admitted=%d refused=%d", len(messages)-refused, refused)
	assert.Less(t, time.Since(finishedAt), 40*time.Second, "ingress releases capacity before the existing 45s ledger bound")
	assert.EqualValues(t, 1, computations.Load())
	assert.EqualValues(t, 1, env.calls.Load())
	job := loadInboundContinuationJob(t, &App{DB: env.shared}, env.org.ID, env.work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	var actions []models.ScheduledJob
	require.NoError(t, env.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
		env.org.ID, inboundContinuationActionJobKind, env.work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2, "computed result and delivered reply both have durable action records")
	for _, action := range actions {
		assert.Equal(t, inboundActionStateResolved, action.Payload["state"])
	}
	require.NoError(t, env.run(t, env.baseTime.Add(time.Second)))
	assert.EqualValues(t, 1, computations.Load(), "duplicate continuation cannot rerun the AI")
	assert.EqualValues(t, 1, env.calls.Load(), "duplicate continuation cannot resend the reply")
	for i, message := range messages {
		work, duplicate, err := env.app.persistAuthenticatedIncomingMessageBeforeAck(
			env.account.PhoneID, message, "Synthetic ingress", strings.Repeat("a", 64))
		require.NoError(t, err)
		require.Equal(t, admitted[i], duplicate)
		if !admitted[i] {
			require.NotNil(t, work)
		}
		_, duplicate, err = env.app.persistAuthenticatedIncomingMessageBeforeAck(
			env.account.PhoneID, message, "Synthetic ingress", strings.Repeat("a", 64))
		require.NoError(t, err)
		require.True(t, duplicate, "same WAMID is accepted only once after capacity returns")
	}
	h1RequireLiveAfterRun(t, env)
}
