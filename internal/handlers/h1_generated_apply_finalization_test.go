package handlers

// Generated-text transaction-boundary regressions. All providers, accounts and
// SQL actors belong to each fixture. Admission remains weight three; the held
// SQL connections witness the physical footprint independently of that charge.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1GeneratedStart(t *testing.T, f *h1CapacityLegacyFixture, release func()) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		if release != nil {
			release()
		}
		f.model.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(20 * time.Second):
			t.Error("generated apply did not join")
		}
		h1IngressJoin(t, f.app)
	})
	go func() {
		defer close(joined)
		p := NewInboundContinuationProcessor(f.app, time.Second)
		done <- p.ProcessMessage(ctx, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	}()
	return done
}

func h1GeneratedResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("generated apply did not return within its fixture boundary")
		return nil
	}
}

func h1GeneratedSentCount(f *h1CapacityLegacyFixture) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func h1GeneratedHistory(t *testing.T, f *h1CapacityLegacyFixture, incoming, outgoing int) {
	t.Helper()
	var rows []models.ChatbotSessionMessage
	require.NoError(t, f.shared.Where("session_id = ?", f.sessions[0].ID).Find(&rows).Error)
	counts := map[models.Direction]int{}
	for _, row := range rows {
		counts[row.Direction]++
		if row.Direction == models.DirectionIncoming {
			require.Equal(t, f.texts[0], row.Message)
		} else {
			require.Equal(t, "Generated: "+f.texts[0], row.Message)
		}
	}
	require.Equal(t, incoming, counts[models.DirectionIncoming])
	require.Equal(t, outgoing, counts[models.DirectionOutgoing])
}

func h1GeneratedResolved(t *testing.T, f *h1CapacityLegacyFixture) models.Message {
	t.Helper()
	work := f.works[0]
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?",
		work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2)
	var outgoing models.Message
	seen := map[string]bool{}
	for _, action := range actions {
		require.Equal(t, models.ScheduledJobStatusCompleted, action.Status)
		require.Equal(t, inboundActionStateResolved, action.Payload["state"])
		require.NotEqual(t, true, action.Payload["manual_review_required"])
		require.Equal(t, "continuation", action.Payload["action_scope"])
		result := inboundContinuationActionResult(action.Payload)
		kind, ok := action.Payload["effect_kind"].(string)
		require.True(t, ok)
		require.False(t, seen[kind], "one durable action per effect")
		seen[kind] = true
		ordinal, ok := inboundContinuationExactInteger(action.Payload["action_ordinal"])
		require.True(t, ok)
		switch kind {
		case "legacy_ai_generate":
			require.Equal(t, 0, ordinal)
			require.Equal(t, "generated", result["outcome"])
			require.Equal(t, "Generated: "+f.texts[0], result["answer"])
		case "text":
			require.Equal(t, 1, ordinal)
			require.NoError(t, f.shared.Where("id = ? AND organization_id = ?", result["outgoing_message_id"], work.OrganizationID).First(&outgoing).Error)
		default:
			t.Fatalf("unexpected generated action kind: %v", action.Payload["effect_kind"])
		}
	}
	require.NotEqual(t, uuid.Nil, outgoing.ID)
	require.Equal(t, work.Contact.ID, outgoing.ContactID)
	require.Equal(t, models.MessageStatusSent, outgoing.Status)
	require.Equal(t, automaticAIDispatchStateResolved, outgoing.Metadata[automaticAIDispatchStateMetadataKey])
	require.NotEmpty(t, outgoing.WhatsAppMessageID)
	require.EqualValues(t, 1, f.model.calls.Load())
	require.Equal(t, 1, h1GeneratedSentCount(f))
	return outgoing
}

func h1GeneratedRetry(t *testing.T, f *h1CapacityLegacyFixture, releaseBeforeJoin ...func()) {
	t.Helper()
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	p := NewInboundContinuationProcessor(f.app, time.Second)
	p.now = func() time.Time { return job.RunAt.Add(time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		for _, release := range releaseBeforeJoin {
			release()
		}
		cancel()
		select {
		case <-joined:
		case <-time.After(20 * time.Second):
			t.Error("generated replay did not join")
		}
		h1IngressJoin(t, f.app)
	})
	go func() {
		defer close(joined)
		done <- p.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
	}()
	require.NoError(t, h1GeneratedResult(t, done))
}

// The subscription has an explicit cancellation/close/join lifecycle; no
// permanently running WebSocket hub is needed for these callback assertions.
type h1GeneratedEvents struct {
	mu     sync.Mutex
	events []queue.RealtimeEvent
	errs   []error
}

func h1GeneratedCapture(t *testing.T, f *h1CapacityLegacyFixture) *h1GeneratedEvents {
	t.Helper()
	require.NotNil(t, f.app.Redis, "focused fixture must provide its synthetic Redis")
	ctx, cancel := context.WithCancel(context.Background())
	sub := f.app.Redis.Subscribe(ctx, queue.RealtimeChannel)
	// Register cleanup before confirming subscription: a failed handshake
	// still owns a subscription and cancellation context.
	t.Cleanup(func() { cancel(); _ = sub.Close() })
	readyCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := sub.Receive(readyCtx)
	readyCancel()
	require.NoError(t, err)
	capture := &h1GeneratedEvents{}
	joined := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = sub.Close()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("generated realtime capture did not join")
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		require.Empty(t, capture.errs)
	})
	go func() {
		defer close(joined)
		for {
			message, err := sub.ReceiveMessage(ctx)
			if err != nil {
				if ctx.Err() == nil {
					capture.mu.Lock()
					capture.errs = append(capture.errs, err)
					capture.mu.Unlock()
				}
				return
			}
			var event queue.RealtimeEvent
			if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
				capture.mu.Lock()
				capture.errs = append(capture.errs, err)
				capture.mu.Unlock()
				continue
			}
			if event.OrganizationID == f.works[0].OrganizationID {
				capture.mu.Lock()
				capture.events = append(capture.events, event)
				capture.mu.Unlock()
			}
		}
	}()
	return capture
}

// Use the existing Hub fixture for actual fallback payloads. Hub.Run has no
// stop API and remains process-lifetime; the isolated test process owns it.
// Its client and this collector are explicitly unregistered and joined.
type h1GeneratedFallback struct {
	mu        sync.Mutex
	envelopes []terminalWSEnvelope
	errs      []error
}

func h1GeneratedFallbackCapture(t *testing.T, f *h1CapacityLegacyFixture) *h1GeneratedFallback {
	t.Helper()
	hub := appwebsocket.NewHub(f.app.Log)
	go hub.Run()
	client := appwebsocket.NewClient(hub, nil, uuid.New(), f.works[0].OrganizationID)
	hub.Register(client)
	capture := &h1GeneratedFallback{}
	joined := make(chan struct{})
	t.Cleanup(func() {
		hub.Unregister(client)
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("generated fallback collector did not join")
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		require.Empty(t, capture.errs)
	})
	go func() {
		defer close(joined)
		for data := range client.SendChan() {
			var envelope terminalWSEnvelope
			err := json.Unmarshal(data, &envelope)
			capture.mu.Lock()
			if err != nil {
				capture.errs = append(capture.errs, err)
			} else {
				capture.envelopes = append(capture.envelopes, envelope)
			}
			capture.mu.Unlock()
		}
	}()
	f.app.WSHub = hub
	return capture
}

func (c *h1GeneratedFallback) outgoing(id uuid.UUID) []terminalWSEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []terminalWSEnvelope
	for _, envelope := range c.envelopes {
		if envelope.Type == appwebsocket.TypeNewMessage && envelope.Payload["id"] == id.String() {
			result = append(result, envelope)
		}
	}
	return result
}

func (c *h1GeneratedEvents) outgoing(id uuid.UUID) []queue.RealtimeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []queue.RealtimeEvent
	for _, event := range c.events {
		if event.MessageID != nil && *event.MessageID == id &&
			(event.Kind == queue.RealtimeEventMessageCreated || event.Kind == queue.RealtimeEventMessageStatusChanged) {
			result = append(result, event)
		}
	}
	return result
}

func h1GeneratedNoOutgoingEvents(t *testing.T, events *h1GeneratedEvents, id uuid.UUID) {
	t.Helper()
	deadline := time.NewTimer(150 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		require.Empty(t, events.outgoing(id), "send callbacks must wait for the finalizer's actual commit")
		select {
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

func TestH1GeneratedApply_PhysicalSendAndResolvedReplayUseTwoConnections(t *testing.T) {
	t.Run("misbound-detached-read", func(t *testing.T) {
		f := h1CapacityLegacy(t, 1)
		for _, kind := range []string{"unguarded", "scoped", "foreign-pool"} {
			execution := f.app.scopedApp(f.app.DB, f.works[0].OrganizationID)
			execution.tenantOrgID = uuid.Nil
			execution.inboundContinuation = &inboundContinuationExecution{
				OrganizationID: f.works[0].OrganizationID, detachedGeneratedApply: true, attemptGuarded: true,
			}
			switch kind {
			case "unguarded":
				execution.inboundContinuation.attemptGuarded = false
			case "scoped":
				execution.tenantOrgID = f.works[0].OrganizationID
			case "foreign-pool":
				execution.DB = f.shared
			}
			called := false
			err := execution.withInboundContinuationDetachedApplyRead(func(*gorm.DB) error { called = true; return nil })
			require.Error(t, err, kind)
			require.False(t, called, kind)
			// Ordinary callers still read their original DB/snapshot; only the
			// explicitly detached, guarded root shape gets a new read scope.
			execution.inboundContinuation.detachedGeneratedApply = false
			require.NoError(t, execution.withInboundContinuationDetachedApplyRead(func(db *gorm.DB) error {
				called = true
				require.Same(t, execution.DB, db, kind)
				return nil
			}))
			require.True(t, called, kind)
		}
		require.Zero(t, f.pool.Stats().InUse)
	})
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-send", true: "resolved-recovery"}[replay], func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			var softFault atomic.Bool
			const sentinel = "synthetic local completion failure before resolved replay"
			callback := "h1:generated-peak-checkpoint:" + uuid.NewString()
			require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if !replay || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" {
					return
				}
				updates, ok := tx.Statement.Dest.(map[string]any)
				if !ok {
					return
				}
				metadata, ok := updates["metadata"].(models.JSONB)
				if ok && metadata["inbound_continuation_completed"] == true && softFault.CompareAndSwap(false, true) {
					_ = tx.AddError(errors.New(sentinel))
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
			if replay {
				f.model.unblock()
				err := h1GeneratedResult(t, h1GeneratedStart(t, f, nil))
				require.ErrorContains(t, err, sentinel)
				require.True(t, softFault.Load())
				require.False(t, inboundContinuationRequiresManualReview(err))
				h1GeneratedResolved(t, f)
				h1GeneratedHistory(t, f, 1, 1)
				// Force the meaningful resolved-send SLA repair path, not merely a
				// completed-parent shortcut. The first read must release its own
				// tenant transaction before the independent restoration transaction.
				require.NoError(t, f.shared.Model(&models.Contact{}).Where("id = ?", f.works[0].Contact.ID).
					Updates(map[string]any{"chatbot_last_message_at": nil, "chatbot_reminder_sent": true}).Error)
				releaseSQL := h1HoldConnections(t, f.pool, 8)
				h1GeneratedRetry(t, f, releaseSQL)
				h1GeneratedResolved(t, f)
				h1GeneratedHistory(t, f, 2, 2) // Existing invocation-local history behavior.
				var contact models.Contact
				require.NoError(t, f.shared.First(&contact, "id = ?", f.works[0].Contact.ID).Error)
				require.NotNil(t, contact.ChatbotLastMessageAt)
				require.False(t, contact.ChatbotReminderSent)
				require.Equal(t, 8, f.pool.Stats().InUse, "only the eight explicit fixture owners remain")
				releaseSQL()
				return
			}

			entered, allow := make(chan struct{}), make(chan struct{})
			var once, enteredOnce sync.Once
			releaseProvider := func() { once.Do(func() { close(allow) }) }
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					To   string `json:"to"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				}
				if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Type != "text" || body.Text.Body != "Generated: "+f.texts[0] {
					f.providerErrors.Add(1)
					http.Error(w, "invalid synthetic generated send", http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.sent = append(f.sent, h1CapacitySent{To: strings.TrimPrefix(body.To, "+"), Body: body.Text.Body, WAMID: "wamid.generated-two-owner"})
				f.mu.Unlock()
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-allow:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.generated-two-owner"}]}`))
			}))
			t.Cleanup(provider.Close)
			f.app.WhatsApp = whatsapp.NewWithBaseURL(f.app.Log, provider.URL)
			var creditReleases []func()
			for range 2 {
				release, err := f.app.acquireInboundContinuationCapacity(context.Background(), nil, 3)
				require.NoError(t, err)
				creditReleases = append(creditReleases, release)
				t.Cleanup(release)
			}
			var releaseSQL func()
			done := h1GeneratedStart(t, f, func() {
				releaseProvider()
				if releaseSQL != nil {
					releaseSQL()
				}
				for _, release := range creditReleases {
					release()
				}
			})
			f.model.unblock()
			h1CapacityWaitQueue(t, &f.app.continuationAdmission, 6, 1)
			require.Zero(t, f.pool.Stats().InUse, "captured apply is queued without SQL ownership")
			releaseSQL = h1HoldConnections(t, f.pool, 8)
			for _, release := range creditReleases {
				release()
			}
			select {
			case <-entered:
			case <-time.After(8 * time.Second):
				t.Fatal("actual provider must start with only two free physical connections")
			}
			require.Equal(t, 10, f.pool.Stats().InUse, "eight fixture owners plus physical guard and provider transaction")
			h1GeneratedHistory(t, f, 0, 0)
			releaseProvider()
			require.NoError(t, h1GeneratedResult(t, done))
			f.assertGenerated(t, 0)
			require.Zero(t, f.providerErrors.Load())
			require.Equal(t, 8, f.pool.Stats().InUse, "all apply SQL scopes close before returning")
			releaseSQL()
		})
	}
}

func TestH1GeneratedApply_HistorySQLFailureRetriesResolvedEffects(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	var injected atomic.Bool
	fault := make(chan error, 1)
	callback := "h1:generated-history-abort:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*models.ChatbotSessionMessage)
		if !ok || row.SessionID != f.sessions[0].ID || row.Direction != models.DirectionIncoming || !injected.CompareAndSwap(false, true) {
			return
		}
		// A real PostgreSQL error poisons this transaction. A plain callback
		// sentinel would not exercise the changed history/verification ordering.
		err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Exec("SELECT 1 / 0").Error
		fault <- err
		_ = tx.AddError(err)
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
	f.model.unblock()
	err := h1GeneratedResult(t, h1GeneratedStart(t, f, nil))
	require.Error(t, err)
	require.False(t, inboundContinuationRequiresManualReview(err), "finalization failed after the independently verified result resolved")
	select {
	case sqlErr := <-fault:
		require.Equal(t, "22012", postgresErrorCode(sqlErr))
	default:
		t.Fatal("actual SQL abort did not fire")
	}
	h1GeneratedResolved(t, f)
	h1GeneratedHistory(t, f, 0, 0)
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, f.works[0].OrganizationID, f.works[0].Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.NotEqual(t, true, job.Payload["manual_review_required"])
	h1GeneratedRetry(t, f)
	f.assertGenerated(t, 0)
}

func TestH1GeneratedApply_GuardFailurePreservesIncomingHistory(t *testing.T) {
	t.Run("fresh-policy-denial", func(t *testing.T) {
		f := h1CapacityLegacy(t, 1)
		var releases []func()
		for range 2 {
			release, err := f.app.acquireInboundContinuationCapacity(context.Background(), nil, 3)
			require.NoError(t, err)
			releases = append(releases, release)
			t.Cleanup(release)
		}
		done := h1GeneratedStart(t, f, func() {
			for _, release := range releases {
				release()
			}
		})
		f.model.unblock()
		h1CapacityWaitQueue(t, &f.app.continuationAdmission, 6, 1)
		work := f.works[0]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		writer := &App{DB: f.shared.WithContext(ctx), Log: f.app.Log}
		transfer := &models.AgentTransfer{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: work.OrganizationID,
			ContactID: work.Contact.ID, WhatsAppAccount: work.Account.Name, PhoneNumber: work.Contact.PhoneNumber,
			Status: models.TransferStatusActive, Source: models.TransferSourceManual, TransferredAt: time.Now().UTC()}
		contact := work.Contact
		require.NoError(t, writer.saveAndFinalizeTransfer(transfer, &work.Account, &contact, nil, true, nil))
		for _, release := range releases {
			release()
		}
		require.NoError(t, h1GeneratedResult(t, done))
		h1GeneratedHistory(t, f, 1, 0)
		require.EqualValues(t, 1, f.model.calls.Load())
		require.Zero(t, h1GeneratedSentCount(f))
		job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
		require.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
		var incoming models.Message
		require.NoError(t, f.shared.First(&incoming, "id = ?", work.Persisted.ID).Error)
		require.Equal(t, true, incoming.Metadata["inbound_continuation_completed"])
		var textActions int64
		require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "text").Count(&textActions).Error)
		require.Zero(t, textActions)
	})
	t.Run("verification-after-acceptance", func(t *testing.T) {
		f := h1CapacityLegacy(t, 1)
		events := h1GeneratedCapture(t, f)
		var injected atomic.Bool
		const sentinel = "synthetic guarded verification read failed after provider acceptance"
		callback := "h1:generated-verify-failure:" + uuid.NewString()
		require.NoError(t, f.app.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Error == nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "messages" &&
				strings.Join(tx.Statement.Selects, ",") == "id,status,error_message" && injected.CompareAndSwap(false, true) {
				_ = tx.AddError(errors.New(sentinel))
			}
		}))
		t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(callback) })
		f.model.unblock()
		err := h1GeneratedResult(t, h1GeneratedStart(t, f, nil))
		require.True(t, injected.Load())
		require.ErrorContains(t, err, "provider attempt did not reach a confirmed sent state")
		require.True(t, inboundContinuationRequiresManualReview(err))
		require.EqualValues(t, 1, f.model.calls.Load())
		require.Equal(t, 1, h1GeneratedSentCount(f))
		h1GeneratedHistory(t, f, 1, 0)
		work := f.works[0]
		var outgoing models.Message
		require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND direction = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing).First(&outgoing).Error)
		require.Equal(t, models.MessageStatusSent, outgoing.Status)
		var textAction models.ScheduledJob
		require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "text").First(&textAction).Error)
		require.Equal(t, inboundActionStateManualReview, textAction.Payload["state"])
		job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
		require.Equal(t, models.ScheduledJobStatusFailed, job.Status)
		require.Eventually(t, func() bool { return len(events.outgoing(outgoing.ID)) == 2 }, 3*time.Second, 10*time.Millisecond,
			"guard error must not strand callbacks when incoming-history finalization commits")
		require.NoError(t, NewInboundContinuationProcessor(f.app, time.Second).ProcessMessage(context.Background(), work.OrganizationID, work.Persisted.ID))
		require.Equal(t, 1, h1GeneratedSentCount(f))
	})
}

func TestH1GeneratedApply_CallbacksWaitForFinalizationCommit(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "sql-rollback"}[rollback], func(t *testing.T) {
			f := h1CapacityLegacy(t, 1)
			events := h1GeneratedCapture(t, f)
			fallback := h1GeneratedFallbackCapture(t, f)
			const rawProfile = "601799123456"
			const maskedProfile = "********3456"
			require.True(t, f.app.Config.Database.RLSEnabled)
			require.NoError(t, f.shared.Model(&models.Organization{}).Where("id = ?", f.works[0].OrganizationID).
				Update("settings", models.JSONB{"mask_phone_numbers": true}).Error)
			require.NoError(t, f.shared.Model(&models.Contact{}).Where("id = ?", f.works[0].Contact.ID).
				Update("profile_name", rawProfile).Error)
			entered, allow := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allow) }) }
			var held atomic.Bool
			fault := make(chan error, 1)
			callback := "h1:generated-finalizer-commit:" + uuid.NewString()
			require.NoError(t, f.app.DB.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
				row, ok := tx.Statement.Dest.(*models.ChatbotSessionMessage)
				if tx.Error != nil || !ok || row.SessionID != f.sessions[0].ID || row.Direction != models.DirectionOutgoing || !held.CompareAndSwap(false, true) {
					return
				}
				close(entered)
				<-allow
				if rollback {
					err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Exec("SELECT 1 / 0").Error
					fault <- err
					_ = tx.AddError(err)
				}
			}))
			t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
			done := h1GeneratedStart(t, f, release)
			f.model.unblock()
			select {
			case <-entered:
			case <-time.After(12 * time.Second):
				t.Fatal("finalizer did not reach uncommitted outgoing history")
			}
			outgoing := h1GeneratedResolved(t, f)
			h1GeneratedHistory(t, f, 0, 0)
			h1GeneratedNoOutgoingEvents(t, events, outgoing.ID)
			require.Empty(t, fallback.outgoing(outgoing.ID), "masked fallback also waits for finalization commit")
			release()
			err := h1GeneratedResult(t, done)
			if rollback {
				require.Error(t, err)
				select {
				case sqlErr := <-fault:
					require.Equal(t, "22012", postgresErrorCode(sqlErr))
				default:
					t.Fatal("finalizer SQL rollback did not fire")
				}
				h1GeneratedHistory(t, f, 0, 0)
				h1GeneratedResolved(t, f)
				h1GeneratedNoOutgoingEvents(t, events, outgoing.ID)
				require.Empty(t, fallback.outgoing(outgoing.ID), "rolled-back finalizer must discard the payload callback")
			} else {
				require.NoError(t, err)
				f.assertGenerated(t, 0)
				require.Eventually(t, func() bool { return len(events.outgoing(outgoing.ID)) == 2 }, 3*time.Second, 10*time.Millisecond)
				got := events.outgoing(outgoing.ID)
				require.Equal(t, queue.RealtimeEventMessageStatusChanged, got[0].Kind)
				require.Equal(t, queue.RealtimeEventMessageCreated, got[1].Kind)
				for _, event := range got {
					require.Equal(t, string(models.MessageStatusSent), event.Status)
				}
				require.Eventually(t, func() bool { return len(fallback.outgoing(outgoing.ID)) == 1 }, 3*time.Second, 10*time.Millisecond)
				payload := fallback.outgoing(outgoing.ID)[0].Payload
				require.Equal(t, maskedProfile, payload["profile_name"])
				require.NotEqual(t, rawProfile, payload["profile_name"])
				require.Equal(t, string(models.MessageStatusSent), payload["status"])
			}
		})
	}
}

func TestH1GeneratedApply_DeletedSessionDoesNotReplayAcceptedResult(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	entered, allow := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(allow) }) }
	var held atomic.Bool
	oldSessionID := f.sessions[0].ID
	callback := "h1:generated-deleted-parent:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		row, ok := tx.Statement.Dest.(*models.ChatbotSessionMessage)
		if ok && row.SessionID == oldSessionID && row.Direction == models.DirectionIncoming && held.CompareAndSwap(false, true) {
			close(entered)
			<-allow
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Create().Remove(callback) })
	done := h1GeneratedStart(t, f, release)
	f.model.unblock()
	select {
	case <-entered:
	case <-time.After(12 * time.Second):
		t.Fatal("accepted send did not reach deferred incoming history")
	}
	outgoing := h1GeneratedResolved(t, f)
	h1GeneratedHistory(t, f, 0, 0)
	deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 3*time.Second)
	deleted := f.shared.WithContext(deleteCtx).Unscoped().Where("id = ? AND organization_id = ?", oldSessionID, f.works[0].OrganizationID).Delete(&models.ChatbotSession{})
	cancelDelete()
	require.NoError(t, deleted.Error)
	require.EqualValues(t, 1, deleted.RowsAffected, "the independent maintenance actor really removes the former FK parent")
	release()
	err := h1GeneratedResult(t, done)
	require.Error(t, err)
	require.False(t, inboundContinuationRequiresManualReview(err), "known accepted result must remain replayable after local history FK failure")
	require.Equal(t, outgoing.ID, h1GeneratedResolved(t, f).ID)
	var staleRows int64
	require.NoError(t, f.shared.Model(&models.ChatbotSessionMessage{}).Where("session_id = ?", oldSessionID).Count(&staleRows).Error)
	require.Zero(t, staleRows)
	h1GeneratedRetry(t, f)
	var replacement models.ChatbotSession
	require.NoError(t, f.shared.Where("organization_id = ? AND contact_id = ? AND status = ?", f.works[0].OrganizationID, f.works[0].Contact.ID, models.SessionStatusActive).First(&replacement).Error)
	require.NotEqual(t, oldSessionID, replacement.ID)
	f.sessions[0] = &replacement
	f.assertGenerated(t, 0)
	require.Equal(t, outgoing.ID, h1GeneratedResolved(t, f).ID)
}
