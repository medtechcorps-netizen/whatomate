package handlers

// Source proposal: these tests have not been compiled or run. They exercise
// genuine legacy model requests through ProcessMessage, not a substituted
// continuation callback. Their transport refuses all unowned external requests.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type h1CapacityModelRequest struct {
	Current  string
	Messages []map[string]string
}

type h1CapacityModelTransport struct {
	entered chan h1CapacityModelRequest
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
	errors  atomic.Int32
}

func (m *h1CapacityModelTransport) unblock() { m.once.Do(func() { close(m.release) }) }

func (m *h1CapacityModelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/chat/completions" {
		m.errors.Add(1)
		return nil, errors.New("unexpected request outside the owned synthetic model endpoint")
	}
	defer r.Body.Close()
	var payload struct {
		Model    string              `json:"model"`
		Messages []map[string]string `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil || payload.Model != "h1-capacity-synthetic" || len(payload.Messages) == 0 {
		m.errors.Add(1)
		return nil, errors.New("invalid synthetic model body")
	}
	current := payload.Messages[len(payload.Messages)-1]
	if current["role"] != "user" || !strings.HasPrefix(current["content"], "Synthetic capacity ") {
		m.errors.Add(1)
		return nil, errors.New("model did not receive the attributed synthetic inbound")
	}
	m.calls.Add(1)
	select {
	case m.entered <- h1CapacityModelRequest{Current: current["content"], Messages: payload.Messages}:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	select {
	case <-m.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "Generated: " + current["content"]}}}})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

type h1CapacitySent struct{ To, Body, WAMID string }

type h1CapacityLegacyFixture struct {
	app            *App
	shared         *gorm.DB
	pool           *sql.DB
	model          *h1CapacityModelTransport
	works          []*persistedIncomingMessage
	sessions       []*models.ChatbotSession
	texts          []string
	mu             sync.Mutex
	sent           []h1CapacitySent
	providerErrors atomic.Int32
}

func h1CapacityLegacy(t *testing.T, count int) *h1CapacityLegacyFixture {
	t.Helper()
	app := newProcessorTestApp(t)
	f := &h1CapacityLegacyFixture{app: app, shared: app.DB}
	f.model = &h1CapacityModelTransport{entered: make(chan h1CapacityModelRequest, count+8), release: make(chan struct{})}
	for i := 0; i < count; i++ {
		organization, account := createProcessorTestOrg(t, app)
		settings := &models.ChatbotSettings{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			WhatsAppAccount: account.Name, IsEnabled: true, SessionTimeoutMins: 30,
			FallbackMessage: "Synthetic fallback must not replace a generated answer",
			AI: models.AIConfig{Enabled: true, Provider: models.AIProviderOpenAI,
				APIKey: "synthetic-not-a-credential", Model: "h1-capacity-synthetic", MaxTokens: 64,
				IncludeHistory: true, HistoryLimit: 8},
		}
		require.NoError(t, f.shared.Create(settings).Error)
		text := fmt.Sprintf("Synthetic capacity %d %s", i, uuid.NewString())
		message := inboundContinuationTextMessage(t, "wamid.capacity-"+uuid.NewString(), fmt.Sprintf("601799%06d", i+1), text)
		work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, message, "Synthetic capacity fixture")
		require.NoError(t, err)
		require.False(t, duplicate)
		session := &models.ChatbotSession{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organization.ID,
			ContactID: work.Contact.ID, WhatsAppAccount: account.Name, PhoneNumber: work.Contact.PhoneNumber,
			Status: models.SessionStatusActive, SessionData: models.JSONB{}, StartedAt: time.Now(), LastActivityAt: time.Now(),
		}
		require.NoError(t, f.shared.Create(session).Error)
		f.works, f.sessions, f.texts = append(f.works, work), append(f.sessions, session), append(f.texts, text)
	}
	private, pool := h1OpenPool(t, 10)
	app.DB, f.pool = private, pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	app.HTTPClient = &http.Client{Transport: f.model, Timeout: 30 * time.Second}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Status string `json:"status"`
			To     string `json:"to"`
			Type   string `json:"type"`
			Text   struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			f.providerErrors.Add(1)
			http.Error(w, "invalid synthetic body", http.StatusBadRequest)
			return
		}
		if payload.Status == "read" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		if payload.Type != "text" || payload.To == "" || payload.Text.Body == "" {
			f.providerErrors.Add(1)
			http.Error(w, "unexpected synthetic send", http.StatusBadRequest)
			return
		}
		sent := h1CapacitySent{To: strings.TrimPrefix(payload.To, "+"), Body: payload.Text.Body, WAMID: "wamid.capacity-sent-" + uuid.NewString()}
		f.mu.Lock()
		f.sent = append(f.sent, sent)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": sent.WAMID}}})
	}))
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	t.Cleanup(provider.Close)
	t.Cleanup(f.model.unblock)
	return f
}

func (f *h1CapacityLegacyFixture) start(t *testing.T, indexes ...int) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	done := make(chan error, len(indexes))
	var wg sync.WaitGroup
	for _, index := range indexes {
		work := f.works[index]
		wg.Add(1)
		go func() {
			defer wg.Done()
			processor := NewInboundContinuationProcessor(f.app, time.Second)
			done <- processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID)
		}()
	}
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	// Cleanup runs before server/pool cleanup even after an assertion fails.
	t.Cleanup(func() {
		f.model.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(20 * time.Second):
			t.Error("capacity continuations did not join")
		}
	})
	return done
}

func h1CapacityJoin(t *testing.T, done <-chan error, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("capacity continuation did not finish")
		}
	}
}

func (f *h1CapacityLegacyFixture) assertGenerated(t *testing.T, index int) {
	t.Helper()
	work := f.works[index]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, job.Status)
	require.Empty(t, job.LastError)
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2)
	byKind := map[string]models.ScheduledJob{}
	for _, action := range actions {
		kind, ok := action.Payload["effect_kind"].(string)
		require.True(t, ok)
		_, duplicate := byKind[kind]
		require.False(t, duplicate)
		byKind[kind] = action
		require.Equal(t, models.ScheduledJobStatusCompleted, action.Status)
		require.Equal(t, inboundActionStateResolved, action.Payload["state"])
		require.NotEqual(t, true, action.Payload["manual_review_required"])
		require.Equal(t, "continuation", action.Payload["action_scope"])
	}
	ai, ok := byKind["legacy_ai_generate"]
	require.True(t, ok)
	text, ok := byKind["text"]
	require.True(t, ok)
	aiOrdinal, ok := inboundContinuationExactInteger(ai.Payload["action_ordinal"])
	require.True(t, ok)
	require.Equal(t, 0, aiOrdinal)
	textOrdinal, ok := inboundContinuationExactInteger(text.Payload["action_ordinal"])
	require.True(t, ok)
	require.Equal(t, 1, textOrdinal)
	answer := "Generated: " + f.texts[index]
	require.Equal(t, "generated", inboundContinuationActionResult(ai.Payload)["outcome"])
	require.Equal(t, answer, inboundContinuationActionResult(ai.Payload)["answer"])
	outgoingID, ok := inboundContinuationActionResult(text.Payload)["outgoing_message_id"].(string)
	require.True(t, ok)
	var outgoing models.Message
	require.NoError(t, f.shared.Where("id = ? AND organization_id = ?", outgoingID, work.OrganizationID).First(&outgoing).Error)
	require.Equal(t, work.Contact.ID, outgoing.ContactID)
	require.Equal(t, models.DirectionOutgoing, outgoing.Direction)
	require.Equal(t, answer, outgoing.Content)
	f.mu.Lock()
	sends := append([]h1CapacitySent(nil), f.sent...)
	f.mu.Unlock()
	matched := 0
	for _, sent := range sends {
		if sent.WAMID == outgoing.WhatsAppMessageID {
			matched++
			require.Equal(t, strings.TrimPrefix(work.Contact.PhoneNumber, "+"), sent.To)
			require.Equal(t, answer, sent.Body)
		}
	}
	require.Equal(t, 1, matched, "the resolved action must refer to the real synthetic provider request")
	var history []models.ChatbotSessionMessage
	require.NoError(t, f.shared.Where("session_id = ?", f.sessions[index].ID).Find(&history).Error)
	var incoming, replies int
	for _, row := range history {
		if row.Direction == models.DirectionIncoming && row.Message == f.texts[index] {
			incoming++
		}
		if row.Direction == models.DirectionOutgoing && row.Message == answer {
			replies++
		}
	}
	require.Equal(t, 1, incoming, "one incoming history row after successful apply")
	require.Equal(t, 1, replies, "one generated outgoing history row")
}

func TestH1Capacity_LegacyParallelCallsReleaseOuterAndReplayExact(t *testing.T) {
	f := h1CapacityLegacy(t, 2)
	done := f.start(t, 0, 1)
	seen := map[string]h1CapacityModelRequest{}
	for i := 0; i < 2; i++ {
		select {
		case request := <-f.model.entered:
			seen[request.Current] = request
		case <-time.After(12 * time.Second):
			t.Fatal("both real legacy model calls must enter before either is released")
		}
	}
	require.Len(t, seen, 2)
	for _, text := range f.texts {
		request, found := seen[text]
		require.True(t, found)
		// Preserve current prompt semantics: history contains the current inbound,
		// and the provider appends the explicit current user message again.
		copies := 0
		for _, message := range request.Messages {
			if message["role"] == "user" && message["content"] == text {
				copies++
			}
		}
		require.Equal(t, 2, copies)
	}
	// Both transport bodies are captured and the calls are blocked. Preparation
	// is over; only two physical AI policy guards may retain connections. This
	// exact ownership assertion fails the old outer+guard transaction shape.
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 2 }, 2*time.Second, 10*time.Millisecond,
		"two blocked legacy model calls must retain two guards, not two extra outer transactions")
	readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, f.app.DB.WithContext(readCtx).Exec("SELECT 1").Error)
	cancelRead()
	for i, work := range f.works {
		var count int64
		require.NoError(t, f.shared.Model(&models.ChatbotSessionMessage{}).Where("session_id = ?", f.sessions[i].ID).Count(&count).Error)
		require.Zero(t, count, "unapplied session history is not durable during model I/O")
		job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
		require.Equal(t, models.ScheduledJobStatusProcessing, job.Status)
	}
	f.model.unblock()
	h1CapacityJoin(t, done, 2)
	for i := range f.works {
		f.assertGenerated(t, i)
	}
	h1CapacityJoin(t, f.start(t, 0, 1), 2)
	for i := range f.works {
		f.assertGenerated(t, i)
	}
	require.EqualValues(t, 2, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 2, sends)
	require.Zero(t, f.model.errors.Load())
	require.Zero(t, f.providerErrors.Load())
	require.Eventually(t, func() bool { return f.pool.Stats().InUse == 0 }, time.Second, 10*time.Millisecond)
}

func TestH1Capacity_ResolvedAIAndAcceptedSendSurviveCompletionRetry(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	const sentinel = "synthetic completion checkpoint rejected once"
	var injected atomic.Bool
	callback := "h1:capacity-completion:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "messages" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		metadata, ok := updates["metadata"].(models.JSONB)
		if !ok || metadata["inbound_continuation_completed"] != true {
			return
		}
		if injected.CompareAndSwap(false, true) {
			_ = tx.AddError(errors.New(sentinel))
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
	f.model.unblock()
	select {
	case err := <-f.start(t, 0):
		require.ErrorContains(t, err, sentinel)
		require.False(t, inboundContinuationRequiresManualReview(err))
	case <-time.After(30 * time.Second):
		t.Fatal("completion failure did not settle")
	}
	require.True(t, injected.Load(), "the known local completion failure must actually fire")
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusPending, job.Status)
	require.Contains(t, job.LastError, sentinel)
	require.NotContains(t, job.LastError, "manual review")
	require.Equal(t, 1, job.Attempts)
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 2)
	for _, action := range actions {
		require.Equal(t, inboundActionStateResolved, action.Payload["state"])
		require.NotEqual(t, true, action.Payload["manual_review_required"])
	}
	processor := NewInboundContinuationProcessor(f.app, time.Second)
	processor.now = func() time.Time { return job.RunAt.Add(time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, processor.ProcessMessage(ctx, work.OrganizationID, work.Persisted.ID))
	finished := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, finished.Status)
	require.EqualValues(t, 1, f.model.calls.Load(), "resolved computation is not recomputed")
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Equal(t, 1, sends, "accepted send is not repeated")
	assert.Zero(t, f.model.errors.Load())
	assert.Zero(t, f.providerErrors.Load())
}

func TestH1Capacity_UnknownAIResultRemainsManualReviewWithoutRecomputation(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	const sentinel = "synthetic generated-result persistence failed once"
	var injected atomic.Bool
	callback := "h1:capacity-result:" + uuid.NewString()
	require.NoError(t, f.app.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "scheduled_jobs" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		payload, ok := updates["payload"].(models.JSONB)
		if !ok || payload["effect_kind"] != "legacy_ai_generate" || payload["state"] != inboundActionStateResolved {
			return
		}
		if injected.CompareAndSwap(false, true) {
			_ = tx.AddError(errors.New(sentinel))
		}
	}))
	t.Cleanup(func() { _ = f.app.DB.Callback().Update().Remove(callback) })
	f.model.unblock()
	select {
	case err := <-f.start(t, 0):
		require.ErrorContains(t, err, sentinel)
		require.True(t, inboundContinuationRequiresManualReview(err))
	case <-time.After(30 * time.Second):
		t.Fatal("uncertain result failure did not settle")
	}
	require.True(t, injected.Load(), "failure follows a real generated model response")
	work := f.works[0]
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusFailed, job.Status)
	require.Equal(t, true, job.Payload["manual_review_required"])
	require.Contains(t, job.LastError, sentinel)
	require.Contains(t, job.LastError, "durable result is uncertain")
	require.Equal(t, 1, job.Attempts, "uncertainty is not a refunded congestion deferral")
	require.Nil(t, job.Payload["wedge_guard_deferrals"])
	var actions []models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID).Find(&actions).Error)
	require.Len(t, actions, 1)
	require.Equal(t, "legacy_ai_generate", actions[0].Payload["effect_kind"])
	require.Equal(t, inboundActionStatePreAttempt, actions[0].Payload["state"], "the failed independent result transaction must not look resolved")
	require.Empty(t, inboundContinuationActionResult(actions[0].Payload))
	h1CapacityJoin(t, f.start(t, 0), 1)
	unchanged := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, job.Version, unchanged.Version)
	require.Equal(t, job.Attempts, unchanged.Attempts)
	require.Equal(t, true, unchanged.Payload["manual_review_required"])
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Zero(t, sends)
	require.Zero(t, f.model.errors.Load())
	require.Zero(t, f.providerErrors.Load())
}

func TestH1Capacity_ManualPauseBetweenGeneratedResultAndSendUsesFreshGuard(t *testing.T) {
	f := h1CapacityLegacy(t, 1)
	done := f.start(t, 0)
	select {
	case <-f.model.entered:
	case <-time.After(12 * time.Second):
		t.Fatal("real model call did not acquire its physical guard")
	}
	work := f.works[0]
	// An independent policy writer is a separate actor, not a connection loaned
	// to the continuation. Its private GORM callbacks cannot affect other tests.
	writerDB, _ := h1OpenPool(t, 2)
	writerEntered := make(chan struct{})
	writerRelease := make(chan struct{})
	var writerReleaseOnce sync.Once
	releaseWriter := func() { writerReleaseOnce.Do(func() { close(writerRelease) }) }
	var held atomic.Bool
	callback := "h1:capacity-pause:" + uuid.NewString()
	require.NoError(t, writerDB.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "agent_transfers" {
			return
		}
		if !held.CompareAndSwap(false, true) {
			return
		}
		close(writerEntered)
		<-writerRelease
	}))
	t.Cleanup(func() { _ = writerDB.Callback().Create().Remove(callback) })
	writerPID := make(chan int, 1)
	writerDone := make(chan error, 1)
	writerJoined := make(chan struct{})
	go func() {
		defer close(writerJoined)
		writerDone <- writerDB.Connection(func(connection *gorm.DB) error {
			var pid int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				writerPID <- 0
				return err
			}
			writerPID <- pid
			writer := &App{DB: connection, Log: f.app.Log}
			transfer := &models.AgentTransfer{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: work.OrganizationID,
				ContactID: work.Contact.ID, WhatsAppAccount: work.Account.Name,
				PhoneNumber: work.Contact.PhoneNumber, Status: models.TransferStatusActive,
				Source: models.TransferSourceManual, TransferredAt: time.Now().UTC(),
			}
			contact := work.Contact
			return writer.saveAndFinalizeTransfer(transfer, &work.Account, &contact, nil, true, nil)
		})
	}()
	t.Cleanup(func() {
		releaseWriter()
		f.model.unblock()
		select {
		case <-writerJoined:
		case <-time.After(20 * time.Second):
			t.Error("policy writer did not join")
		}
	})
	var pid int
	select {
	case pid = <-writerPID:
	case <-time.After(5 * time.Second):
		t.Fatal("policy writer did not obtain a connection")
	}
	require.Positive(t, pid)
	testutil.RequirePostgresBackendWaitingForLock(t, f.shared, pid)
	var before int64
	require.NoError(t, f.shared.Model(&models.AgentTransfer{}).Where("contact_id = ? AND organization_id = ? AND status = ?", work.Contact.ID, work.OrganizationID, models.TransferStatusActive).Count(&before).Error)
	require.Zero(t, before, "manual Pause cannot commit while the model's physical guard remains held")
	f.model.unblock()
	select {
	case <-writerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("manual Pause did not acquire its writer fence after the model guard ended")
	}
	// The writer now owns the organization fence and has made its local update,
	// but has not committed. The captured generated result must already be durable.
	var action models.ScheduledJob
	require.NoError(t, f.shared.Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?",
		work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "legacy_ai_generate").First(&action).Error)
	require.Equal(t, inboundActionStateResolved, action.Payload["state"])
	require.Equal(t, "generated", inboundContinuationActionResult(action.Payload)["outcome"])
	f.mu.Lock()
	beforeCommitSends := len(f.sent)
	f.mu.Unlock()
	require.Zero(t, beforeCommitSends)
	releaseWriter()
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("manual Pause did not commit")
	}
	h1CapacityJoin(t, done, 1)
	var after int64
	require.NoError(t, f.shared.Model(&models.AgentTransfer{}).Where("contact_id = ? AND organization_id = ? AND status = ?", work.Contact.ID, work.OrganizationID, models.TransferStatusActive).Count(&after).Error)
	require.EqualValues(t, 1, after)
	job := loadInboundContinuationJob(t, &App{DB: f.shared}, work.OrganizationID, work.Persisted.ID)
	require.Equal(t, models.ScheduledJobStatusCompleted, job.Status, "fresh policy denial is successful terminal consumption")
	require.NotEqual(t, true, job.Payload["manual_review_required"])
	h1CapacityJoin(t, f.start(t, 0), 1)
	require.EqualValues(t, 1, f.model.calls.Load())
	f.mu.Lock()
	sends := len(f.sent)
	f.mu.Unlock()
	require.Zero(t, sends, "the answer computed before manual Pause must not bypass fresh send authorization")
	var outgoing int64
	require.NoError(t, f.shared.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ?", work.OrganizationID, work.Contact.ID, models.DirectionOutgoing).Count(&outgoing).Error)
	require.Zero(t, outgoing)
	var textActions int64
	require.NoError(t, f.shared.Model(&models.ScheduledJob{}).Where("organization_id = ? AND kind = ? AND aggregate_id = ? AND payload->>'effect_kind' = ?", work.OrganizationID, inboundContinuationActionJobKind, work.Persisted.ID, "text").Count(&textActions).Error)
	require.Zero(t, textActions, "policy denial precedes the text claim")
}
