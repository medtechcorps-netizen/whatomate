package handlers

// H1 follow-up regressions (fixer pass after the 1b15155 reviews):
//   - a bounded-wait retry keeps the new-session greeting (durable session origin);
//   - the job's terminal writes ignore an expired fast-path context;
//   - a pre-claim completion-webhook timeout is retried before session completion;
//   - a pre-dispatch native-booking deferral is not turned into manual review;
//   - in-fence best-effort writes wait 2s for a connection, not 45s;
//   - phase 1 and the getOrCreateSession fence bound their organization-fence wait
//     (slow, H1_SLOW_TESTS=1).
// Failure-injection callbacks are registered only on dedicated pools (h1OpenPool),
// never on the shared test *gorm.DB. None of these tests calls t.Parallel.

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// h1SyntheticWedge has the exact shape of an H1 bounded-wait expiry.
func h1SyntheticWedge(site string) error {
	return fmt.Errorf("wedge guard: synthetic %s: %w", site, context.DeadlineExceeded)
}

func h1JobDeferrals(job models.ScheduledJob) int {
	count, _ := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
	return count
}

// h1FailLockedMessageQueries fails the listed occurrences (1-based) of a locked
// (FOR ...) Message query on db only, returning the occurrence counter.
func h1FailLockedMessageQueries(t *testing.T, db *gorm.DB, name string, failure error, occurrences ...int32) *atomic.Int32 {
	t.Helper()
	fail := make(map[int32]bool, len(occurrences))
	for _, occurrence := range occurrences {
		fail[occurrence] = true
	}
	var seen atomic.Int32
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if _, isMessage := tx.Statement.Dest.(*models.Message); !isMessage {
			return
		}
		if _, locked := tx.Statement.Clauses["FOR"]; !locked {
			return
		}
		if fail[seen.Add(1)] {
			_ = tx.AddError(failure)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
	return &seen
}

// MAJOR (liveness review): getOrCreateSession commits the new session on its own,
// so a wedge-guard retry of the SAME inbound message used to see an existing
// session and silently skip the DefaultResponse greeting (completing with no
// reply, or with the fallback). The session now records its creating inbound.
func TestInboundContinuation_NewSessionGreetingSurvivesWedgeRetry(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	require.NoError(t, shared.Create(&models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		WhatsAppAccount:    account.Name,
		IsEnabled:          true,
		DefaultResponse:    "Welcome to the H1 clinic",
		FallbackMessage:    "H1 fallback for an existing session",
		SessionTimeoutMins: 30,
		AI:                 models.AIConfig{Enabled: false},
	}).Error)
	phone := "6056" + uuid.NewString()[:8]
	first, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.h1-greeting-1-"+uuid.NewString(), phone, "Hello there"), "Greeting patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	second, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.h1-greeting-2-"+uuid.NewString(), phone, "A second question"), "Greeting patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, first.Contact.ID, second.Contact.ID)

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	calls := h1CountingProvider(t, app, http.StatusOK)
	// The greeting's action claim is the run's first ledger write: expire it like a
	// claim pool wait would, after getOrCreateSession has committed the session.
	h1FailFirstCreate(t, pool, "h1:fail_first_greeting_claim", h1IsActionLedgerCreate,
		h1SyntheticWedge("greeting claim pool wait"))
	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }

	err = processor.ProcessMessage(context.Background(), organization.ID, first.Persisted.ID)
	require.ErrorContains(t, err, "send chatbot greeting")
	h1RequireWedgeGuard(t, err)
	assert.Zero(t, calls.Load())
	job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, first.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 0, job.Attempts)
	assert.Equal(t, 1, h1JobDeferrals(job))
	var sessions []models.ChatbotSession
	require.NoError(t, shared.Where("organization_id = ? AND contact_id = ? AND status = ?",
		organization.ID, first.Contact.ID, models.SessionStatusActive).Find(&sessions).Error)
	require.Len(t, sessions, 1, "run 1 committed its new session independently of the continuation")
	assert.Equal(t, first.Persisted.ID.String(), sessions[0].SessionData["__inbound_session_origin"])

	require.NoError(t, pool.Callback().Create().Remove("h1:fail_first_greeting_claim"))
	processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, first.Persisted.ID))
	assert.EqualValues(t, 1, calls.Load(), "the retry must send the greeting exactly once")
	outgoing := h1OutgoingMessages(t, shared, organization.ID, first.Contact.ID)
	require.Len(t, outgoing, 1)
	assert.Equal(t, "Welcome to the H1 clinic", outgoing[0].Content)
	job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, first.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)

	// The marker belongs to the creating inbound only: the next message on the same
	// session is an existing-session message (fallback), never a second greeting.
	processor.now = func() time.Time { return time.Now().UTC().Add(time.Minute) }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, second.Persisted.ID))
	assert.EqualValues(t, 2, calls.Load())
	outgoing = h1OutgoingMessages(t, shared, organization.ID, first.Contact.ID)
	require.Len(t, outgoing, 2)
	assert.Equal(t, "H1 fallback for an existing session", outgoing[1].Content)
}

// MAJOR (harness): p.complete / p.fail ran on the fast path's 2-minute context. A
// run that outlived it left the job 'processing' until its 5-minute lease expired.
func TestInboundContinuation_TerminalWritesIgnoreExpiredRunContext(t *testing.T) {
	for name, outcome := range map[string]error{
		"complete": nil,
		"fail":     fmt.Errorf("synthetic failure after the run deadline"),
	} {
		t.Run(name, func(t *testing.T) {
			app := newProcessorTestApp(t)
			organization, account := createProcessorTestOrg(t, app)
			work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
				inboundContinuationTextMessage(t, "wamid.h1-deadline-"+uuid.NewString(), "6057"+uuid.NewString()[:8], "Slow run"),
				"Deadline patient")
			require.NoError(t, err)
			require.False(t, duplicate)
			processor := NewInboundContinuationProcessor(app, time.Second)
			processor.process = func(ctx context.Context, _ *App, _ *persistedIncomingMessage) error {
				<-ctx.Done() // the run outlives the fast path's deadline
				return outcome
			}
			runCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			err = processor.ProcessMessage(runCtx, organization.ID, work.Persisted.ID)
			job := loadInboundContinuationJob(t, app, organization.ID, work.Persisted.ID)
			if outcome == nil {
				require.NoError(t, err)
				assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
			} else {
				require.ErrorContains(t, err, "synthetic failure after the run deadline")
				assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
				assert.Empty(t, job.LockedBy, "the failure must release the lease, not wait for it to go stale")
			}
		})
	}
}

// MAJOR (durability review): a bounded wait that expired before the completion
// webhook's claim committed was logged and the session completed, so the tenant's
// webhook was never called. It is now retried before the session-complete commit.
func TestRunChatGraph_CompletionWebhookPreClaimTimeoutRetries(t *testing.T) {
	app, org, account, contact, session := newGraphTestFixtures(t)
	shared := app.DB
	var webhookCalls atomic.Int32
	app.HTTPClient = graphExternalAPIClient(func(_ *http.Request) (int, string, error) {
		webhookCalls.Add(1)
		return http.StatusNoContent, "", nil
	})
	flow := completionWebhookFlow(models.JSONB{
		"url":    "https://hooks.example.test/chatbot-completed",
		"method": "POST",
	})
	flow.OrganizationID = org.ID
	flow.WhatsAppAccount = account.Name
	execution := *app.inboundContinuation

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	h1FailFirstCreate(t, pool, "h1:fail_first_completion_claim", h1IsActionLedgerCreate,
		h1SyntheticWedge("completion webhook claim pool wait"))

	err := app.runChatGraph(account, contact, session, flow, "start", "", nil)
	h1RequireWedgeGuard(t, err)
	assert.Zero(t, webhookCalls.Load())
	var stored models.ChatbotSession
	require.NoError(t, shared.First(&stored, "id = ?", session.ID).Error)
	assert.Equal(t, models.SessionStatusActive, stored.Status, "the session must not complete before its webhook")
	assert.Zero(t, h1ActionLedgerRows(t, shared, org.ID, execution.MessageID))

	// The retry is the same inbound delivery: same durable identity.
	retry := execution
	app.inboundContinuation = &retry
	require.NoError(t, app.runChatGraph(account, contact, &stored, flow, "start", "", nil))
	assert.EqualValues(t, 1, webhookCalls.Load())
	require.NoError(t, shared.First(&stored, "id = ?", session.ID).Error)
	assert.Equal(t, models.SessionStatusCompleted, stored.Status)
}

// MAJOR (durability review): any error from a native booking dispatch marked the
// prepared message for staff review, so a refunded wedge-guard retry ended in
// manual review with no automatic booking reply. Before the dispatch marker
// commits the booking is demonstrably unattempted and must be retried cleanly;
// after it, the staff-review marker is still written.
func TestNativeAIBooking_WedgeGuardDeferralBeforeDispatchRetries(t *testing.T) {
	t.Run("before_dispatch_marker_is_retried_without_review", func(t *testing.T) {
		f := nativeBookingFixture(t)
		calls := nativePausePolicyProvider(t, f.app, nil)
		prepared, _ := f.offer(t, false)
		pool, _ := h1OpenPool(t, 10)
		f.app.DB = pool
		// Occurrence 1 is the preflight's message lock, inside the same transaction
		// that would commit the dispatch marker.
		h1FailLockedMessageQueries(t, pool, "h1:fail_booking_preflight", h1SyntheticWedge("booking preflight pool wait"), 1)

		err := f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact)
		h1RequireWedgeGuard(t, err)
		assert.False(t, inboundContinuationRequiresManualReview(err))
		assert.Zero(t, calls.Load())
		var pending models.Message
		require.NoError(t, f.app.DB.First(&pending, "id = ?", prepared.Message.ID).Error)
		assert.Nil(t, pending.Metadata["ai_booking_manual_confirmation_required"])
		assert.Nil(t, pending.Metadata[automaticAIDispatchStateMetadataKey])
		var job models.ScheduledJob
		require.NoError(t, f.app.DB.First(&job, "id = ?", prepared.Claim.ID).Error)
		assert.Equal(t, inboundActionStateBookingPrepared, job.Payload["state"])
		assert.Zero(t, job.Attempts)

		require.NoError(t, pool.Callback().Query().Remove("h1:fail_booking_preflight"))
		id := f.identity(t)
		var reloaded *nativeBookingPrepared
		require.NoError(t, f.app.DB.Transaction(func(tx *gorm.DB) error {
			var loadErr error
			reloaded, loadErr = f.app.loadNativeBookingPreparedTx(tx, id)
			return loadErr
		}))
		require.True(t, reloaded.Claim.Execute)
		require.NoError(t, f.app.dispatchNativeBookingPrepared(reloaded, f.account, f.contact))
		assert.EqualValues(t, 1, calls.Load(), "the retried booking reply is sent exactly once")
		require.NoError(t, f.app.DB.First(&pending, "id = ?", prepared.Message.ID).Error)
		assert.Equal(t, models.MessageStatusSent, pending.Status)
		assert.Nil(t, pending.Metadata["ai_booking_manual_confirmation_required"])
	})

	t.Run("after_dispatch_marker_still_requires_review", func(t *testing.T) {
		f := nativeBookingFixture(t)
		calls := nativePausePolicyProvider(t, f.app, nil)
		prepared, _ := f.offer(t, false)
		pool, _ := h1OpenPool(t, 10)
		f.app.DB = pool
		// Occurrences 2 and 3 are the post-provider settlement and its reconcile.
		h1FailLockedMessageQueries(t, pool, "h1:fail_booking_settle", h1SyntheticWedge("booking settle pool wait"), 2, 3)

		err := f.app.dispatchNativeBookingPrepared(prepared, f.account, f.contact)
		h1RequireWedgeGuard(t, err)
		assert.EqualValues(t, 1, calls.Load())
		var pending models.Message
		require.NoError(t, f.app.DB.First(&pending, "id = ?", prepared.Message.ID).Error)
		assert.Equal(t, true, pending.Metadata["ai_booking_manual_confirmation_required"],
			"a committed dispatch marker means the provider may have been called")
		id := f.identity(t)
		_, loadErr := f.app.loadNativeBookingPreparedTx(f.app.DB, id)
		assert.True(t, inboundContinuationRequiresManualReview(loadErr))
	})
}

// MAJOR (liveness review): inside the AI-attempt fence the goroutine holds the
// organization KEY SHARE; its best-effort mark-read and reminder bookkeeping now
// wait at most 2s for a pooled connection (previously the 45s in-fence budget each).
func TestH1InFenceBestEffortWritesWaitBriefly(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, shared, organization.ID, testutil.WithContactAccount(account.Name))
	incoming := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.h1-unread-" + uuid.NewString(),
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "unread inbound",
		Status:            models.MessageStatusReceived,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, shared.Create(&incoming).Error)

	pool, sqlDB := h1OpenPool(t, 3)
	root := h1RootApp(pool, true)
	nested := h1NestedApp(root, organization.ID)
	nested.inboundContinuation.ContactID = contact.ID
	nested.inboundContinuation.attemptGuarded = true
	request := OutgoingMessageRequest{Account: account, Contact: contact}
	options := MessageSendOptions{MarkIncomingRead: true}
	reply := &models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, MessageType: models.MessageTypeText}
	writes := []struct {
		name  string
		write func()
	}{
		{"mark_read", func() { nested.finalizeMessageSend(reply, request, options, "wamid.h1-reply", nil, false) }},
		{"chatbot_tracking", func() { nested.UpdateContactChatbotMessage(contact.ID) }},
	}

	release := h1HoldConnections(t, sqlDB, 3)
	for _, write := range writes {
		started := time.Now()
		write.write()
		elapsed := time.Since(started)
		assert.GreaterOrEqual(t, elapsed, 1900*time.Millisecond, write.name)
		assert.Less(t, elapsed, 5*time.Second, "%s must not keep the AI-attempt fence for the 45s in-fence budget", write.name)
	}
	release()

	for _, write := range writes {
		write.write()
	}
	var stored models.Message
	require.NoError(t, shared.First(&stored, "id = ?", incoming.ID).Error)
	assert.Equal(t, models.MessageStatusRead, stored.Status)
	var tracked models.Contact
	require.NoError(t, shared.First(&tracked, "id = ?", contact.ID).Error)
	assert.NotNil(t, tracked.ChatbotLastMessageAt)
}

// MAJOR (harness + liveness reviews): phase 1 held a pooled connection while it
// waited, unbounded, for the organization fence behind in-flight AI guards; with a
// guard holder waiting for the pool that closed a pool+lock cycle. The wait is now
// a 20s-bounded, refunded deferral that runs nothing of the continuation.
func TestInboundContinuation_PhaseOneFenceWaitIsBoundedDeferral(t *testing.T) {
	h1RequireSlow(t)
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID,
		inboundContinuationTextMessage(t, "wamid.h1-phase1-"+uuid.NewString(), "6058"+uuid.NewString()[:8], "Fenced"),
		"Fence patient")
	require.NoError(t, err)
	require.False(t, duplicate)

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }
	var invoked atomic.Int32
	processor.process = func(context.Context, *App, *persistedIncomingMessage) error {
		invoked.Add(1)
		return nil
	}

	policyWriter := shared.Begin()
	require.NoError(t, policyWriter.Error)
	t.Cleanup(func() { policyWriter.Rollback() })
	require.NoError(t, policyWriter.Exec(
		"SELECT id FROM organizations WHERE id = ? FOR UPDATE", organization.ID,
	).Error)

	started := time.Now()
	err = processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
	elapsed := time.Since(started)
	h1RequireWedgeGuard(t, err)
	assert.Contains(t, err.Error(), "organization policy fence")
	assert.GreaterOrEqual(t, elapsed, 19*time.Second)
	assert.LessOrEqual(t, elapsed, 25*time.Second)
	assert.Zero(t, invoked.Load())
	job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 0, job.Attempts)
	assert.Equal(t, 1, h1JobDeferrals(job))

	require.NoError(t, policyWriter.Rollback().Error)
	processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
	assert.EqualValues(t, 1, invoked.Load())
	job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

// The getOrCreateSession bound now covers its wait FOR the organization fence too
// (it pins the continuation transaction and its own connection while it waits).
func TestGetOrCreateSession_FenceWaitBounded(t *testing.T) {
	h1RequireSlow(t)
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, shared, organization.ID, testutil.WithContactAccount(account.Name))

	policyWriter := shared.Begin()
	require.NoError(t, policyWriter.Error)
	t.Cleanup(func() { policyWriter.Rollback() })
	require.NoError(t, policyWriter.Exec(
		"SELECT id FROM organizations WHERE id = ? FOR UPDATE", organization.ID,
	).Error)

	execution := app.scopedApp(shared, organization.ID)
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: organization.ID,
		ContactID:      contact.ID,
		MessageID:      uuid.New(),
		WAMID:          "wamid.h1-session-fence-" + uuid.NewString(),
	}
	started := time.Now()
	_, _, err := execution.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	elapsed := time.Since(started)
	h1RequireWedgeGuard(t, err)
	assert.Contains(t, err.Error(), "fence")
	assert.GreaterOrEqual(t, elapsed, 19*time.Second)
	assert.LessOrEqual(t, elapsed, 25*time.Second)
	var sessions int64
	require.NoError(t, shared.Model(&models.ChatbotSession{}).
		Where("organization_id = ? AND contact_id = ?", organization.ID, contact.ID).Count(&sessions).Error)
	assert.Zero(t, sessions)
}
