package handlers

// H1 pool-wedge hotfix: continuation-level regressions. Cases that need a
// GORM failure callback or a constrained pool switch the App to a dedicated
// pool (h1OpenPool) after fixtures are created through the shared test pool,
// so no callback is ever registered on the shared *gorm.DB. None of these
// tests calls t.Parallel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// h1CountingProvider is a WhatsApp Cloud API stand-in that counts sends.
func h1CountingProvider(t *testing.T, app *App, status int) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.WriteHeader(status)
		if status >= http.StatusBadRequest {
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic provider failure"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.h1-provider.%s.%d", uuid.NewString()[:8], call)}},
		})
	}))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	return &calls
}

// h1FailFirstCreate registers a one-shot GORM create failure on db only.
func h1FailFirstCreate(t *testing.T, db *gorm.DB, name string, match func(*gorm.DB) bool, failure error) *atomic.Bool {
	t.Helper()
	var armed atomic.Bool
	armed.Store(true)
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if match(tx) && armed.CompareAndSwap(true, false) {
			_ = tx.AddError(failure)
		}
	}))
	return &armed
}

func h1IsAgentTransferCreate(tx *gorm.DB) bool {
	_, ok := tx.Statement.Dest.(*models.AgentTransfer)
	return ok
}

func h1IsActionLedgerCreate(tx *gorm.DB) bool {
	job, ok := tx.Statement.Dest.(*models.ScheduledJob)
	return ok && job.Kind == inboundContinuationActionJobKind
}

func h1ActiveTransfers(t *testing.T, db *gorm.DB, organizationID, contactID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.AgentTransfer{}).Where(
		"organization_id = ? AND contact_id = ? AND status = ?",
		organizationID, contactID, models.TransferStatusActive,
	).Count(&count).Error)
	return count
}

func h1OutgoingMessages(t *testing.T, db *gorm.DB, organizationID, contactID uuid.UUID) []models.Message {
	t.Helper()
	var outgoing []models.Message
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		organizationID, contactID, models.DirectionOutgoing,
	).Order("created_at").Find(&outgoing).Error)
	return outgoing
}

func h1ActionLedgerRows(t *testing.T, db *gorm.DB, organizationID, messageID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.ScheduledJob{}).Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		organizationID, inboundContinuationActionJobKind, messageID,
	).Count(&count).Error)
	return count
}

func h1DumpGoroutines(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 8<<20)
	dump := buffer[:runtime.Stack(buffer, true)]
	path := filepath.Join(t.TempDir(), "goroutines.txt")
	if err := os.WriteFile(path, dump, 0o600); err == nil {
		t.Logf("goroutine dump written to %s", path)
	}
	return string(dump)
}

// h1AllDaysClosed is a business-hours table that is closed on every day.
func h1AllDaysClosed() models.JSONBArray {
	hours := make(models.JSONBArray, 0, 7)
	for day := 0; day < 7; day++ {
		hours = append(hours, map[string]any{
			"day": float64(day), "enabled": false, "start_time": "09:00", "end_time": "17:00",
		})
	}
	return hours
}

// D1: the inbound broadcast reads (phone mask, reply preview) run on the
// continuation's own transaction. With a single pooled connection held by
// phase 2, the old root-pool read blocked forever.
func TestInboundContinuation_BroadcastUsesOwnTransaction(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	phone := "6051" + uuid.NewString()[:8]
	contact := testutil.CreateTestContactWith(t, shared, organization.ID,
		testutil.WithContactAccount(account.Name), testutil.WithPhoneNumber(phone))
	require.NoError(t, shared.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}).Error)
	message := inboundContinuationTextMessage(t, "wamid.h1-d1-"+uuid.NewString(), phone, "Show this inbound live")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "D1 patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.Equal(t, true, work.Persisted.Metadata[incomingAutomaticAISuppressedKey])

	hub := appwebsocket.NewHub(app.Log)
	go hub.Run()
	app.WSHub = hub
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organization.ID)
	hub.Register(client)
	testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second,
		"inbound realtime client registered")
	var redisMessages <-chan *redis.Message
	if app.Redis != nil {
		subscription := app.Redis.Subscribe(context.Background(), queue.RealtimeChannel)
		t.Cleanup(func() { _ = subscription.Close() })
		_, err = subscription.Receive(context.Background())
		require.NoError(t, err)
		redisMessages = subscription.Channel()
	}

	pool, _ := h1OpenPool(t, 1)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	processor := NewInboundContinuationProcessor(app, time.Second)
	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
	}()
	select {
	case processErr := <-result:
		require.NoError(t, processErr)
	case <-time.After(5 * time.Second):
		h1DumpGoroutines(t)
		t.Fatal("inbound broadcast waited for a second pooled connection while phase 2 held the only one")
	}

	local := receiveInboundContinuationLocalEnvelope(t, client.SendChan())
	assert.Equal(t, appwebsocket.TypeNewMessage, local.Type)
	assert.Equal(t, work.Persisted.ID.String(), local.MessageID)
	assertNoInboundContinuationLocalEnvelope(t, client.SendChan())
	if redisMessages != nil {
		remote := receiveInboundContinuationRedisEvent(t, redisMessages)
		assert.Equal(t, queue.RealtimeEventMessageCreated, remote.Kind)
		require.NotNil(t, remote.MessageID)
		assert.Equal(t, work.Persisted.ID, *remote.MessageID)
		assertNoInboundContinuationRedisEvent(t, redisMessages)
	}
	stored := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
}

// h1EnsureWhatsAppResolver installs the production phone resolver used by
// withPhoneTenant under RLS when the schema does not already carry it, and
// removes it again only if this test installed it.
func h1EnsureWhatsAppResolver(t *testing.T, db *gorm.DB) {
	t.Helper()
	var present bool
	require.NoError(t, db.Raw(
		"SELECT to_regprocedure('public.rereply_resolve_whatsapp_org(text)') IS NOT NULL",
	).Scan(&present).Error)
	if present {
		return
	}
	require.NoError(t, db.Exec(`CREATE OR REPLACE FUNCTION public.rereply_resolve_whatsapp_org(p_phone_id text)
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.whatsapp_accounts
		   WHERE BTRIM(phone_id) = BTRIM(p_phone_id)
		     AND deleted_at IS NULL
		 $function$`).Error)
	t.Cleanup(func() {
		db.Exec("DROP FUNCTION IF EXISTS public.rereply_resolve_whatsapp_org(text)")
	})
}

// D2: a reaction gets no outer phone-tenant transaction, so its goroutine
// never holds one pooled connection while its own tenant phase waits for
// another.
func TestReaction_HoldsAtMostOneConnection(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	shared := app.DB
	h1EnsureWhatsAppResolver(t, shared)
	wamid := "wamid.h1-d2-" + uuid.NewString()
	target := models.Message{
		BaseModel:      models.BaseModel{ID: uuid.NewSHA1(account.ID, []byte("coexistence-message:"+wamid))},
		OrganizationID: account.OrganizationID, ContactID: contact.ID,
		WhatsAppAccount: account.Name, WhatsAppMessageID: wamid,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "reaction target", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, shared.Create(&target).Error)
	var reaction IncomingTextMessage
	require.NoError(t, json.Unmarshal([]byte(`{
		"from": "`+contact.PhoneNumber+`",
		"id": "wamid.h1-d2-reaction-`+uuid.NewString()+`",
		"timestamp": "`+strconv.FormatInt(time.Now().Unix(), 10)+`",
		"type": "reaction",
		"reaction": {"message_id": "`+wamid+`", "emoji": "👍"}
	}`), &reaction))
	require.NotNil(t, reaction.Reaction)

	pool, _ := h1OpenPool(t, 1)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.processIncomingMessage(account.PhoneID, reaction, "Patient")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		dump := h1DumpGoroutines(t)
		t.Fatalf("reaction did not finish with MaxOpenConns=1; a nested pooled acquisition remains:\n%s",
			h1GoroutinesMentioning(dump, "handleIncomingReactionEvent", "resolveWhatsAppMessage", "processIncomingMessage"))
	}
	assert.Equal(t, []string{"\U0001F44D"}, storedReactionEmojis(t, &App{DB: shared}, target.ID))
}

func h1GoroutinesMentioning(dump string, needles ...string) string {
	var selected []string
	for _, stack := range strings.Split(dump, "\n\n") {
		for _, needle := range needles {
			if strings.Contains(stack, needle) {
				selected = append(selected, stack)
				break
			}
		}
	}
	return strings.Join(selected, "\n\n")
}

// h1ChatbotDisabledFixture persists one inbound message for an organization
// whose chatbot is disabled and moves the App to a dedicated pool.
func h1ChatbotDisabledFixture(t *testing.T, hours models.BusinessHoursConfig) (
	*App, *gorm.DB, *models.Organization, *persistedIncomingMessage,
) {
	t.Helper()
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	require.NoError(t, shared.Create(&models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		WhatsAppAccount:    account.Name,
		IsEnabled:          false,
		SessionTimeoutMins: 30,
		BusinessHours:      hours,
		AI:                 models.AIConfig{Enabled: false},
	}).Error)
	message := inboundContinuationTextMessage(t, "wamid.h1-disabled-"+uuid.NewString(),
		"6052"+uuid.NewString()[:8], "Hello team")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Disabled patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	return app, shared, organization, work
}

// The chatbot-disabled handover no longer completes the continuation when
// the transfer phase failed (createTransferToQueue swallows that error).
func TestInboundContinuation_ChatbotDisabledTransferFailureRetries(t *testing.T) {
	t.Run("transfer_insert_failure_is_retried", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, models.BusinessHoursConfig{})
		h1FailFirstCreate(t, app.DB, "h1:fail_first_transfer", h1IsAgentTransferCreate,
			errors.New("h1 synthetic transfer insert failure"))
		baseTime := time.Now().UTC()
		processor := NewInboundContinuationProcessor(app, time.Second)
		processor.now = func() time.Time { return baseTime }

		err := processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
		require.ErrorContains(t, err, "chatbot-disabled queue transfer was not committed")
		// The swallowed transfer error is presumed congestion (the bounded transfer
		// phase): a refunded deferral, so agent-only orgs cannot exhaust MaxAttempts.
		h1RequireWedgeGuard(t, err)
		job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
		assert.Equal(t, 0, job.Attempts, "a handover post-check failure is a refunded deferral")
		deferrals, _ := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
		assert.Equal(t, 1, deferrals)
		assert.Zero(t, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))

		require.NoError(t, app.DB.Callback().Create().Remove("h1:fail_first_transfer"))
		processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
		require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
		job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
		assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))
	})

	closed := models.BusinessHoursConfig{
		Enabled: true, Hours: h1AllDaysClosed(), OutOfHoursMessage: "We are closed right now",
	}

	t.Run("out_of_hours_sends_once_without_transfer", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, closed)
		calls := h1CountingProvider(t, app, http.StatusOK)
		processor := NewInboundContinuationProcessor(app, time.Second)
		require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
		assert.EqualValues(t, 1, calls.Load())
		assert.Zero(t, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))
		outgoing := h1OutgoingMessages(t, shared, organization.ID, work.Contact.ID)
		require.Len(t, outgoing, 1)
		assert.Equal(t, "We are closed right now", outgoing[0].Content)
		job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	})

	t.Run("out_of_hours_pre_dispatch_failure_is_retried", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, closed)
		calls := h1CountingProvider(t, app, http.StatusOK)
		h1FailFirstCreate(t, app.DB, "h1:fail_first_ledger", h1IsActionLedgerCreate, context.DeadlineExceeded)
		baseTime := time.Now().UTC()
		processor := NewInboundContinuationProcessor(app, time.Second)
		processor.now = func() time.Time { return baseTime }

		err := processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
		require.ErrorContains(t, err, "send chatbot-disabled out-of-hours response")
		job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusPending, job.Status, "a failed out-of-hours reply must be retried, not completed")
		assert.Zero(t, calls.Load())

		processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
		require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
		assert.EqualValues(t, 1, calls.Load())
		assert.Zero(t, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))
		job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	})

	t.Run("out_of_hours_provider_failure_is_not_completed", func(t *testing.T) {
		app, shared, organization, work := h1ChatbotDisabledFixture(t, closed)
		calls := h1CountingProvider(t, app, http.StatusServiceUnavailable)
		processor := NewInboundContinuationProcessor(app, time.Second)
		err := processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
		require.Error(t, err)
		assert.EqualValues(t, 1, calls.Load())
		job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
		// A provider attempt is never replayed automatically: it ends in manual
		// review instead of the old silent completion without a reply.
		assert.Equal(t, models.ScheduledJobStatusFailed, job.Status)
		assert.Equal(t, true, job.Payload["manual_review_required"])
	})
}

// The keyword handover is retried when its transfer did not commit, and the
// already-sent transfer body is replayed from the action ledger, not resent.
func TestInboundContinuation_KeywordTransferFailureRetriesWithoutResend(t *testing.T) {
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	require.NoError(t, shared.Create(&models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		WhatsAppAccount:    account.Name,
		IsEnabled:          true,
		SessionTimeoutMins: 30,
		AI:                 models.AIConfig{Enabled: false},
	}).Error)
	require.NoError(t, shared.Create(&models.KeywordRule{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		WhatsAppAccount: account.Name,
		Name:            "h1-keyword-transfer",
		Keywords:        models.StringArray{"agent"},
		MatchType:       models.MatchTypeExact,
		ResponseType:    models.ResponseTypeTransfer,
		ResponseContent: models.JSONB{"body": "Connecting you to an agent"},
		Priority:        10,
		IsEnabled:       true,
	}).Error)
	message := inboundContinuationTextMessage(t, "wamid.h1-keyword-"+uuid.NewString(),
		"6053"+uuid.NewString()[:8], "agent")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Keyword patient")
	require.NoError(t, err)
	require.False(t, duplicate)

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	calls := h1CountingProvider(t, app, http.StatusOK)
	h1FailFirstCreate(t, pool, "h1:fail_first_keyword_transfer", h1IsAgentTransferCreate,
		errors.New("h1 synthetic transfer insert failure"))
	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }

	err = processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
	require.ErrorContains(t, err, "keyword transfer was not committed")
	h1RequireWedgeGuard(t, err)
	assert.EqualValues(t, 1, calls.Load())
	job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	assert.Equal(t, 0, job.Attempts, "a handover post-check failure is a refunded deferral")
	deferrals, _ := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
	assert.Equal(t, 1, deferrals)
	assert.Zero(t, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))

	require.NoError(t, pool.Callback().Create().Remove("h1:fail_first_keyword_transfer"))
	processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
	require.NoError(t, processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID))
	assert.EqualValues(t, 1, calls.Load(), "the transfer body must be replayed from the ledger, never resent")
	assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, organization.ID, work.Contact.ID))
	assert.Len(t, h1OutgoingMessages(t, shared, organization.ID, work.Contact.ID), 1)
	job = loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
}

// A transfer node whose body send hit a bounded wait (DeadlineExceeded)
// before dispatch returns before the session-complete commit, so the retry
// sends the body once and then completes the session and creates the transfer.
func TestExecChatTransfer_BodyTimeoutRetriesBeforeSessionComplete(t *testing.T) {
	app, org, account, contact, session := newGraphTestFixtures(t)
	shared := app.DB
	flow := newTransferFlow(t, app, org, account, map[string]any{"body": "Connecting you to a human."})
	execution := *app.inboundContinuation

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	calls := h1CountingProvider(t, app, http.StatusOK)
	h1FailFirstCreate(t, pool, "h1:fail_first_transfer_body_ledger", h1IsActionLedgerCreate, context.DeadlineExceeded)

	err := app.runChatGraph(account, contact, session, flow, "help", "", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, calls.Load())
	var stored models.ChatbotSession
	require.NoError(t, shared.First(&stored, "id = ?", session.ID).Error)
	assert.Equal(t, models.SessionStatusActive, stored.Status, "the session must not complete before the body is sent")
	assert.Zero(t, h1ActiveTransfers(t, shared, org.ID, contact.ID))
	assert.Zero(t, h1ActionLedgerRows(t, shared, org.ID, execution.MessageID))

	// The retry is the same inbound delivery: same durable identity.
	retry := execution
	app.inboundContinuation = &retry
	require.NoError(t, app.runChatGraph(account, contact, &stored, flow, "help", "", nil))
	assert.EqualValues(t, 1, calls.Load())
	require.NoError(t, shared.First(&stored, "id = ?", session.ID).Error)
	assert.Equal(t, models.SessionStatusCompleted, stored.Status)
	assert.EqualValues(t, 1, h1ActiveTransfers(t, shared, org.ID, contact.ID))
	assert.Len(t, h1OutgoingMessages(t, shared, org.ID, contact.ID), 1)
}

// p.fail refunds attempts for "wedge guard" congestion (bounded, at most 16
// deferrals per job) and counts everything else as before.
func TestInboundContinuationFail_WedgeGuardDeferral(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }
	wedge := fmt.Errorf("get inbound continuation chatbot session: %w",
		fmt.Errorf("wedge guard: no pooled database connection within 15s while this goroutine may hold another: %w",
			context.DeadlineExceeded))

	claimWith := func(t *testing.T, attempts int, payload models.JSONB) (*models.ScheduledJob, uuid.UUID) {
		t.Helper()
		message := inboundContinuationTextMessage(t, "wamid.h1-defer-"+uuid.NewString(),
			"6054"+uuid.NewString()[:8], "Defer me")
		work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Deferral patient")
		require.NoError(t, err)
		require.False(t, duplicate)
		stored := loadInboundContinuationJob(t, app, organization.ID, work.Persisted.ID)
		updates := map[string]any{"attempts": attempts, "run_at": baseTime.Add(-time.Second)}
		if payload != nil {
			merged := cloneInboundContinuationJSONB(stored.Payload)
			for key, value := range payload {
				merged[key] = value
			}
			updates["payload"] = merged
		}
		require.NoError(t, app.DB.Model(&models.ScheduledJob{}).Where("id = ?", stored.ID).Updates(updates).Error)
		job, err := processor.claim(context.Background(), organization.ID, &work.Persisted.ID)
		require.NoError(t, err)
		require.NotNil(t, job)
		return job, work.Persisted.ID
	}
	reload := func(t *testing.T, messageID uuid.UUID) models.ScheduledJob {
		t.Helper()
		return loadInboundContinuationJob(t, app, organization.ID, messageID)
	}
	deferrals := func(job models.ScheduledJob) (int, bool) {
		return inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
	}

	t.Run("wedge_guard_refunds_attempt", func(t *testing.T) {
		job, messageID := claimWith(t, 0, nil)
		require.Equal(t, 1, job.Attempts)
		require.Error(t, processor.fail(context.Background(), organization.ID, job, wedge))
		stored := reload(t, messageID)
		assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
		assert.Equal(t, 0, stored.Attempts)
		count, ok := deferrals(stored)
		assert.True(t, ok)
		assert.Equal(t, 1, count)
		assert.WithinDuration(t, baseTime.Add(inboundContinuationBackoff(1)), stored.RunAt, time.Millisecond)
	})

	t.Run("after_sixteen_deferrals_attempt_counts", func(t *testing.T) {
		job, messageID := claimWith(t, 0, models.JSONB{"wedge_guard_deferrals": 16})
		require.Error(t, processor.fail(context.Background(), organization.ID, job, wedge))
		stored := reload(t, messageID)
		assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
		assert.Equal(t, 1, stored.Attempts)
		count, _ := deferrals(stored)
		assert.Equal(t, 16, count)
		assert.WithinDuration(t, baseTime.Add(inboundContinuationBackoff(17)), stored.RunAt, time.Millisecond)
	})

	t.Run("at_max_attempts_wedge_guard_stays_pending", func(t *testing.T) {
		job, messageID := claimWith(t, defaultInboundContinuationMaxTry-1, models.JSONB{"wedge_guard_deferrals": 3})
		require.Equal(t, defaultInboundContinuationMaxTry, job.Attempts)
		require.Error(t, processor.fail(context.Background(), organization.ID, job, wedge))
		stored := reload(t, messageID)
		assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
		assert.Equal(t, defaultInboundContinuationMaxTry-1, stored.Attempts)
		count, _ := deferrals(stored)
		assert.Equal(t, 4, count)
	})

	t.Run("manual_review_is_terminal", func(t *testing.T) {
		job, messageID := claimWith(t, 0, nil)
		cause := errors.Join(
			&inboundContinuationManualReviewError{ActionKey: "inbound-action:h1", Reason: "synthetic uncertain dispatch"},
			wedge,
		)
		require.Error(t, processor.fail(context.Background(), organization.ID, job, cause))
		stored := reload(t, messageID)
		assert.Equal(t, models.ScheduledJobStatusFailed, stored.Status)
		assert.Equal(t, true, stored.Payload["manual_review_required"])
		_, deferred := deferrals(stored)
		assert.False(t, deferred)
	})

	for name, cause := range map[string]error{
		"deadline_without_marker": fmt.Errorf("plain timeout: %w", context.DeadlineExceeded),
		"marker_without_deadline": errors.New("wedge guard: text only"),
	} {
		t.Run(name, func(t *testing.T) {
			job, messageID := claimWith(t, 0, nil)
			require.Error(t, processor.fail(context.Background(), organization.ID, job, cause))
			stored := reload(t, messageID)
			assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
			assert.Equal(t, 1, stored.Attempts)
			_, deferred := deferrals(stored)
			assert.False(t, deferred)
		})
	}
}

// A policy writer holding the organization fence makes the AI-attempt guard
// lock wait expire after 30 s as a retryable "wedge guard" error before any
// action claim. Previously this was a policy stop after 120 s that completed
// the job and silently dropped the reply.
func TestAIAttemptGuard_LockWaitIsRetryableNotPolicyStop(t *testing.T) {
	h1RequireSlow(t)
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	calls := h1CountingProvider(t, app, http.StatusOK)
	message := inboundContinuationTextMessage(t, "wamid.h1-guard-"+uuid.NewString(),
		"6055"+uuid.NewString()[:8], "Hello")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Guard patient")
	require.NoError(t, err)
	require.False(t, duplicate)

	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }

	policyWriter := shared.Begin()
	require.NoError(t, policyWriter.Error)
	t.Cleanup(func() { policyWriter.Rollback() })
	var sendErr error
	var elapsed time.Duration
	processor.process = func(_ context.Context, scoped *App, work *persistedIncomingMessage) error {
		// Phase 1 has committed; take the fence like a policy writer would.
		if err := policyWriter.Exec(
			"SELECT id FROM organizations WHERE id = ? FOR UPDATE", work.OrganizationID,
		).Error; err != nil {
			return err
		}
		started := time.Now()
		sendErr = scoped.sendAndSaveTextMessage(&work.Account, &work.Contact, "Generated reply")
		elapsed = time.Since(started)
		return sendErr
	}

	err = processor.ProcessMessage(context.Background(), organization.ID, work.Persisted.ID)
	require.Error(t, err)
	h1RequireWedgeGuard(t, sendErr)
	assert.False(t, inboundContinuationStoppedByPolicy(sendErr), "a fence timeout must never be a policy stop")
	assert.GreaterOrEqual(t, elapsed, 29*time.Second)
	assert.LessOrEqual(t, elapsed, 35*time.Second)
	assert.Zero(t, calls.Load())
	assert.Zero(t, h1ActionLedgerRows(t, shared, organization.ID, work.Persisted.ID))
	job := loadInboundContinuationJob(t, &App{DB: shared}, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusPending, job.Status)
	count, _ := inboundContinuationExactInteger(job.Payload["wedge_guard_deferrals"])
	assert.Equal(t, 1, count)
	assert.Equal(t, 0, job.Attempts, "a wedge-guard deferral is refunded")
}

// The nested getOrCreateSession transaction bounds its waits after the
// organization fence (20 s), so a session row share-locked by another
// continuation's open transaction (the session-log FK) cannot hold the fence
// indefinitely; the fence is released as soon as the bound expires.
func TestGetOrCreateSession_PostFenceLockWaitBounded(t *testing.T) {
	h1RequireSlow(t)
	app := newProcessorTestApp(t)
	shared := app.DB
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContactWith(t, shared, organization.ID, testutil.WithContactAccount(account.Name))

	var foreignKeys int64
	require.NoError(t, shared.Raw(`SELECT count(*) FROM pg_catalog.pg_constraint
		WHERE contype = 'f'
		  AND conrelid = 'public.chatbot_session_messages'::regclass
		  AND confrelid = 'public.chatbot_sessions'::regclass`).Scan(&foreignKeys).Error)
	require.Positive(t, foreignKeys, "chatbot_session_messages must reference chatbot_sessions")

	now := time.Now().UTC()
	session := models.ChatbotSession{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ContactID:       contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     contact.PhoneNumber,
		Status:          models.SessionStatusActive,
		SessionData:     models.JSONB{},
		StartedAt:       now,
		LastActivityAt:  now,
	}
	require.NoError(t, shared.Create(&session).Error)

	holder := shared.Begin()
	require.NoError(t, holder.Error)
	t.Cleanup(func() { holder.Rollback() })
	require.NoError(t, holder.Create(&models.ChatbotSessionMessage{
		BaseModel: models.BaseModel{ID: uuid.New()},
		SessionID: session.ID,
		Direction: models.DirectionIncoming,
		Message:   "held by another continuation",
		StepName:  "keyword_check",
	}).Error)

	execution := app.scopedApp(shared, organization.ID)
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: organization.ID,
		ContactID:      contact.ID,
		MessageID:      uuid.New(),
		WAMID:          "wamid.h1-session-" + uuid.NewString(),
	}
	started := time.Now()
	_, _, err := execution.getOrCreateSession(organization.ID, contact.ID, account.Name, contact.PhoneNumber, 30)
	elapsed := time.Since(started)
	h1RequireWedgeGuard(t, err)
	assert.GreaterOrEqual(t, elapsed, 19*time.Second)
	assert.LessOrEqual(t, elapsed, 25*time.Second)

	fenceCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, shared.WithContext(fenceCtx).Transaction(func(tx *gorm.DB) error {
		return tx.Exec("SELECT id FROM organizations WHERE id = ? FOR UPDATE", organization.ID).Error
	}), "the expired session transaction must have released the organization fence")
}

// Source-scan regression guard: the nested root acquisitions H1 bounded must
// not silently reappear as raw root-pool transactions.
func TestH1SourceScan_NoUnboundedNestedRootTransactions(t *testing.T) {
	rawRoot := regexp.MustCompile(`(WithTenant|WithTenantReadCommitted)\(\s*a\.rootApp\(\)\.DB`)
	for _, file := range []string{
		"chatbot_processor.go", "chatbot_graph_runner.go", "native_ai_booking.go", "inbound_continuation.go",
	} {
		source, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Empty(t, rawRoot.FindAllString(string(source), -1), "%s opens a raw root-pool tenant transaction", file)
	}
	for _, file := range []string{"sla_processor.go", "inbound_continuation.go"} {
		source, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.NotContains(t, string(source), "a.rootApp().WithCommittedTenantApp(",
			"%s re-enters WithCommittedTenantApp through the root receiver", file)
	}
}
