package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
)

func inboundContinuationTextMessage(
	t *testing.T,
	wamid, from, body string,
) IncomingTextMessage {
	t.Helper()
	var message IncomingTextMessage
	require.NoError(t, json.Unmarshal([]byte(`{
		"from": "`+from+`",
		"id": "`+wamid+`",
		"timestamp": "1722222222",
		"type": "text",
		"text": {"body": "`+body+`"}
	}`), &message))
	return message
}

func loadInboundContinuationJob(
	t *testing.T,
	app *App,
	organizationID, messageID uuid.UUID,
) models.ScheduledJob {
	t.Helper()
	var job models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		organizationID,
		inboundContinuationJobKind,
		messageID,
	).First(&job).Error)
	return job
}

func TestInboundContinuation_CommittedNativeInboundPublishesRealtimeExactlyOnce(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	if app.Redis == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.realtime-commit-"+uuid.NewString(),
		"6050"+uuid.NewString()[:8],
		"Show this inbound message live",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Realtime patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	// Stop immediately after the native inbound broadcast. This avoids an
	// unrelated chatbot reply or transfer adding another realtime event.
	require.NoError(t, app.DB.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ContactID:       work.Contact.ID,
		WhatsAppAccount: account.Name,
		PhoneNumber:     work.Contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}).Error)

	hub := appwebsocket.NewHub(app.Log)
	go hub.Run()
	app.WSHub = hub
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organization.ID)
	hub.Register(client)
	testutil.AssertEventually(
		t,
		func() bool { return hub.GetClientCount() == 1 },
		2*time.Second,
		"inbound realtime client registered",
	)

	redisSubscription := app.Redis.Subscribe(context.Background(), queue.RealtimeChannel)
	t.Cleanup(func() { _ = redisSubscription.Close() })
	_, err = redisSubscription.Receive(context.Background())
	require.NoError(t, err)
	redisMessages := redisSubscription.Channel()

	// Enable the same outer tenant transaction and after-commit drain used by
	// production. The test database owner may bypass row policies, but the
	// transaction-local tenant and callback lifecycle are identical.
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	processor := NewInboundContinuationProcessor(app, time.Second)
	require.NoError(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))

	local := receiveInboundContinuationLocalEnvelope(t, client.SendChan())
	assert.Equal(t, appwebsocket.TypeNewMessage, local.Type)
	assert.Equal(t, work.Persisted.ID.String(), local.MessageID)
	remote := receiveInboundContinuationRedisEvent(t, redisMessages)
	assert.Equal(t, queue.RealtimeEventMessageCreated, remote.Kind)
	require.NotNil(t, remote.MessageID)
	assert.Equal(t, work.Persisted.ID, *remote.MessageID)
	assert.Equal(t, organization.ID, remote.OrganizationID)

	assertNoInboundContinuationLocalEnvelope(t, client.SendChan())
	assertNoInboundContinuationRedisEvent(t, redisMessages)
	stored := loadInboundContinuationJob(t, app, organization.ID, work.Persisted.ID)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
}

func TestInboundContinuation_RolledBackNativeInboundPublishesNoRealtime(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	if app.Redis == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.realtime-rollback-"+uuid.NewString(),
		"6051"+uuid.NewString()[:8],
		"Never advertise a rollback",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Rollback patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	hub := appwebsocket.NewHub(app.Log)
	go hub.Run()
	app.WSHub = hub
	client := appwebsocket.NewClient(hub, nil, uuid.New(), organization.ID)
	hub.Register(client)
	testutil.AssertEventually(
		t,
		func() bool { return hub.GetClientCount() == 1 },
		2*time.Second,
		"rollback realtime client registered",
	)
	redisSubscription := app.Redis.Subscribe(context.Background(), queue.RealtimeChannel)
	t.Cleanup(func() { _ = redisSubscription.Close() })
	_, err = redisSubscription.Receive(context.Background())
	require.NoError(t, err)
	redisMessages := redisSubscription.Channel()

	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.process = func(
		_ context.Context,
		execution *App,
		loaded *persistedIncomingMessage,
	) error {
		execution.broadcastNewMessage(
			loaded.OrganizationID,
			&loaded.Persisted,
			&loaded.Contact,
		)
		// PostgreSQL marks the transaction aborted. processClaimed deliberately
		// returns nil from its callback to retain safe action checkpoints, so the
		// failed COMMIT is the rollback boundary under test.
		return execution.DB.Exec(
			"SELECT * FROM rereply_forced_missing_inbound_realtime_table",
		).Error
	}
	require.Error(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))

	assertNoInboundContinuationLocalEnvelope(t, client.SendChan())
	assertNoInboundContinuationRedisEvent(t, redisMessages)
}

type inboundContinuationLocalEnvelope struct {
	Type      string         `json:"type"`
	MessageID string         `json:"-"`
	Payload   map[string]any `json:"payload"`
}

func receiveInboundContinuationLocalEnvelope(
	t *testing.T,
	messages <-chan []byte,
) inboundContinuationLocalEnvelope {
	t.Helper()
	select {
	case data := <-messages:
		var envelope inboundContinuationLocalEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		envelope.MessageID, _ = envelope.Payload["id"].(string)
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for committed inbound local realtime")
		return inboundContinuationLocalEnvelope{}
	}
}

func receiveInboundContinuationRedisEvent(
	t *testing.T,
	messages <-chan *redis.Message,
) queue.RealtimeEvent {
	t.Helper()
	select {
	case message := <-messages:
		var event queue.RealtimeEvent
		require.NoError(t, json.Unmarshal([]byte(message.Payload), &event))
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for committed inbound Redis realtime")
		return queue.RealtimeEvent{}
	}
}

func assertNoInboundContinuationLocalEnvelope(
	t *testing.T,
	messages <-chan []byte,
) {
	t.Helper()
	select {
	case data := <-messages:
		t.Fatalf("received duplicate or rolled-back local realtime: %s", data)
	case <-time.After(200 * time.Millisecond):
	}
}

func assertNoInboundContinuationRedisEvent(
	t *testing.T,
	messages <-chan *redis.Message,
) {
	t.Helper()
	select {
	case message := <-messages:
		t.Fatalf("received duplicate or rolled-back Redis realtime: %s", message.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInboundContinuation_CrashLeaseIsReclaimedAndDuplicateWakesJob(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	wamid := "wamid.continuation-crash-" + uuid.NewString()
	message := inboundContinuationTextMessage(
		t,
		wamid,
		"6016"+uuid.NewString()[:8],
		"Please continue my booking",
	)

	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Crash recovery patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)

	baseTime := time.Now().UTC()
	crashedWorker := NewInboundContinuationProcessor(app, time.Second)
	crashedWorker.now = func() time.Time { return baseTime }
	claimed, err := crashedWorker.claim(
		context.Background(),
		organization.ID,
		&work.Persisted.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, models.ScheduledJobStatusProcessing, claimed.Status)
	assert.Equal(t, 1, claimed.Attempts)

	// A Meta redelivery must preserve the live lease while still ensuring that
	// the unfinished durable job exists.
	replayed, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Crash recovery patient",
	)
	require.NoError(t, err)
	require.True(t, duplicate)
	require.NotNil(t, replayed)
	assert.Equal(t, work.Persisted.ID, replayed.Persisted.ID)

	var processed int
	replacement := NewInboundContinuationProcessor(app, time.Second)
	replacement.now = func() time.Time {
		return baseTime.Add(defaultInboundContinuationLease + time.Second)
	}
	replacement.process = func(
		_ context.Context,
		_ *App,
		recovered *persistedIncomingMessage,
	) error {
		processed++
		require.Equal(t, organization.ID, recovered.OrganizationID)
		require.Equal(t, work.Persisted.ID, recovered.Persisted.ID)
		require.Equal(t, wamid, recovered.Message.ID)
		require.Equal(t, "Please continue my booking", recovered.Extracted.Text)
		return nil
	}
	require.NoError(t, replacement.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Equal(t, 1, processed)

	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
	assert.Equal(t, 2, stored.Attempts)
	assert.Empty(t, stored.LockedBy)
	assert.Nil(t, stored.LockedAt)
	require.NotNil(t, stored.CompletedAt)

	// Completed work is never resurrected by another duplicate delivery.
	_, duplicate, err = app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Crash recovery patient",
	)
	require.NoError(t, err)
	require.True(t, duplicate)
	require.NoError(t, replacement.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Equal(t, 1, processed)

	var count int64
	require.NoError(t, app.DB.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND kind = ? AND aggregate_id = ?",
			organization.ID,
			inboundContinuationJobKind,
			work.Persisted.ID,
		).
		Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestInboundContinuation_FailureRetriesWithSameDurableJob(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.continuation-retry-"+uuid.NewString(),
		"6017"+uuid.NewString()[:8],
		"Retry my flow",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Retry patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }
	attempts := 0
	processor.process = func(
		_ context.Context,
		_ *App,
		_ *persistedIncomingMessage,
	) error {
		attempts++
		if attempts == 1 {
			return errors.New("simulated replica interruption")
		}
		return nil
	}

	err = processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	)
	require.ErrorContains(t, err, "simulated replica interruption")
	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
	assert.WithinDuration(t, baseTime.Add(time.Second), stored.RunAt, time.Millisecond)
	assert.Empty(t, stored.LockedBy)

	processor.now = func() time.Time {
		return baseTime.Add(2 * time.Second)
	}
	require.NoError(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Equal(t, 2, attempts)
	stored = loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
	assert.Equal(t, 2, stored.Attempts)
}

func TestInboundContinuation_CriticalProcessingErrorIsRetried(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.continuation-critical-"+uuid.NewString(),
		"6019"+uuid.NewString()[:8],
		"Continue after settings recover",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Critical retry patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }
	err = processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	)
	require.ErrorContains(t, err, "chatbot settings")

	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)
	assert.Equal(t, 1, stored.Attempts)

	createChatbotSettings(
		t,
		app,
		organization.ID,
		account.Name,
		models.AIConfig{Enabled: false},
	)
	processor.now = func() time.Time {
		return baseTime.Add(2 * time.Second)
	}
	require.NoError(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	stored = loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
	assert.Equal(t, 2, stored.Attempts)
}

func TestInboundContinuation_HeartbeatPreventsConcurrentLeaseReplay(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.continuation-heartbeat-"+uuid.NewString(),
		"6020"+uuid.NewString()[:8],
		"Hold the lease",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Heartbeat patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	started := make(chan struct{})
	release := make(chan struct{})
	first := NewInboundContinuationProcessor(app, time.Second)
	first.lease = 120 * time.Millisecond
	first.process = func(
		_ context.Context,
		_ *App,
		_ *persistedIncomingMessage,
	) error {
		close(started)
		<-release
		return nil
	}
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first.ProcessMessage(
			context.Background(),
			organization.ID,
			work.Persisted.ID,
		)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first continuation worker did not start")
	}
	time.Sleep(2 * first.lease)

	var replacementCalls int
	replacement := NewInboundContinuationProcessor(app, time.Second)
	replacement.lease = first.lease
	replacement.process = func(
		_ context.Context,
		_ *App,
		_ *persistedIncomingMessage,
	) error {
		replacementCalls++
		return nil
	}
	require.NoError(t, replacement.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Zero(t, replacementCalls, "a renewed lease must not be replayed")

	close(release)
	require.NoError(t, <-firstResult)
	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
}

func TestInboundContinuation_ResponseActionMarkerSkipsReplay(t *testing.T) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	inboundMessageID := uuid.New()
	wamid := "wamid.action-marker-" + uuid.NewString()

	first := app.scopedApp(app.DB, organization.ID)
	first.inboundContinuation = &inboundContinuationExecution{
		MessageID: inboundMessageID,
		WAMID:     wamid,
	}
	require.NoError(t, first.sendAndSaveTextMessage(
		account,
		contact,
		"One durable response",
	))

	replay := app.scopedApp(app.DB, organization.ID)
	replay.inboundContinuation = &inboundContinuationExecution{
		MessageID: inboundMessageID,
		WAMID:     wamid,
	}
	require.NoError(t, replay.sendAndSaveTextMessage(
		account,
		contact,
		"One durable response",
	))

	var messages []models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		organization.ID,
		contact.ID,
		models.DirectionOutgoing,
	).Find(&messages).Error)
	require.Len(t, messages, 1)
	assert.Equal(t, models.MessageStatusSent, messages[0].Status)
	assert.NotEmpty(
		t,
		messages[0].Metadata["inbound_continuation_action_key"],
	)
	assert.Equal(
		t,
		inboundMessageID.String(),
		messages[0].Metadata["inbound_continuation_message_id"],
	)
	assert.Equal(
		t,
		wamid,
		messages[0].Metadata["inbound_continuation_wamid"],
	)

	var actions []models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		organization.ID,
		inboundContinuationActionJobKind,
		inboundMessageID,
	).Find(&actions).Error)
	require.Len(t, actions, 1)
	assert.Equal(t, models.ScheduledJobStatusCompleted, actions[0].Status)
	assert.Equal(t, inboundActionStateResolved, actions[0].Payload["state"])
	assert.NotContains(t, actions[0].Payload, "body")
	assert.NotContains(t, actions[0].Payload, "headers")
	assert.NotContains(t, actions[0].Payload, "url")
	assert.NotContains(t, actions[0].Payload, "token")
}

func TestInboundContinuation_CompletionCheckpointSkipsWholeJobReplay(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.completion-checkpoint-"+uuid.NewString(),
		"6021"+uuid.NewString()[:8],
		"Do not replay completed actions",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Checkpoint patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	checkpointApp := app.scopedApp(app.DB, organization.ID)
	checkpointApp.inboundContinuation = &inboundContinuationExecution{
		MessageID: work.Persisted.ID,
		WAMID:     message.ID,
	}
	require.NoError(t, checkpointApp.markInboundContinuationCompleted(
		work.Persisted.ID,
	))

	var processCalls int
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.process = func(
		_ context.Context,
		_ *App,
		_ *persistedIncomingMessage,
	) error {
		processCalls++
		return nil
	}
	require.NoError(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Zero(t, processCalls)
	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
}

func TestInboundContinuation_ProviderAttemptFailureIsTerminalWithoutReplay(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)

	var providerCalls atomic.Int32
	failingProvider := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			providerCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
		},
	))
	t.Cleanup(failingProvider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, failingProvider.URL)

	execution := app.scopedApp(app.DB, organization.ID)
	execution.inboundContinuation = &inboundContinuationExecution{
		MessageID: uuid.New(),
		WAMID:     "wamid.provider-failure-" + uuid.NewString(),
	}
	err := execution.sendAndSaveTextMessage(
		account,
		contact,
		"Retry this provider response",
	)
	require.Error(t, err)
	assert.True(t, inboundContinuationRequiresManualReview(err))
	assert.Equal(t, int32(1), providerCalls.Load())

	var failed models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		organization.ID,
		contact.ID,
		models.DirectionOutgoing,
	).Order("created_at DESC").First(&failed).Error)
	assert.Equal(t, models.MessageStatusFailed, failed.Status)
	assert.NotEmpty(
		t,
		failed.Metadata["inbound_continuation_action_key"],
	)

	replay := app.scopedApp(app.DB, organization.ID)
	replay.inboundContinuation = &inboundContinuationExecution{
		MessageID: execution.inboundContinuation.MessageID,
		WAMID:     execution.inboundContinuation.WAMID,
	}
	err = replay.sendAndSaveTextMessage(
		account,
		contact,
		"Retry this provider response",
	)
	require.Error(t, err)
	assert.True(t, inboundContinuationRequiresManualReview(err))
	assert.Equal(
		t,
		int32(1),
		providerCalls.Load(),
		"an attempted provider delivery must never be replayed automatically",
	)

	var action models.ScheduledJob
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND kind = ? AND aggregate_id = ?",
		organization.ID,
		inboundContinuationActionJobKind,
		execution.inboundContinuation.MessageID,
	).First(&action).Error)
	assert.Equal(t, models.ScheduledJobStatusFailed, action.Status)
	assert.Equal(t, inboundActionStateManualReview, action.Payload["state"])
	assert.Equal(t, true, action.Payload["manual_review_required"])
}

func TestInboundContinuation_RLSTenantReplySendsWithoutSelfDeadlock(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			call := providerCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]string{
					{"id": fmt.Sprintf("wamid.rls.%d", call)},
				},
			})
		},
	))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)

	settings := models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     organization.ID,
		WhatsAppAccount:    account.Name,
		IsEnabled:          true,
		DefaultResponse:    "Welcome from the durable chatbot",
		SessionTimeoutMins: 30,
		AI:                 models.AIConfig{Enabled: false},
	}
	require.NoError(t, app.DB.Create(&settings).Error)

	message := inboundContinuationTextMessage(
		t,
		"wamid.rls-no-deadlock-"+uuid.NewString(),
		"6022"+uuid.NewString()[:8],
		"Hello",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"RLS patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	// Enable the production transaction shape only after fixture creation.
	// The processor now holds the same contact/session locks as production,
	// while inbound SendOutgoingMessage deliberately reuses that tenant tx.
	app.Config = &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true},
	}
	processor := NewInboundContinuationProcessor(app, time.Second)
	result := make(chan error, 1)
	go func() {
		result <- processor.ProcessMessage(
			context.Background(),
			organization.ID,
			work.Persisted.ID,
		)
	}()

	select {
	case processErr := <-result:
		require.NoError(t, processErr)
	case <-time.After(5 * time.Second):
		t.Fatal(
			"RLS-shaped inbound chatbot reply self-deadlocked on the contact lock",
		)
	}
	assert.Equal(t, int32(1), providerCalls.Load())

	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)

	var outgoing []models.Message
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		organization.ID,
		work.Contact.ID,
		models.DirectionOutgoing,
	).Find(&outgoing).Error)
	require.Len(t, outgoing, 1)
	assert.Equal(t, models.MessageStatusSent, outgoing[0].Status)

	// A second recovery pass has no claimable continuation and cannot call
	// Meta. Duplicate webhook enqueue behavior is covered separately without
	// relying on the production resolver SQL function in this unit schema.
	require.NoError(t, processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	))
	assert.Equal(t, int32(1), providerCalls.Load())
}

func TestInboundContinuation_RLSOuterRollbackDoesNotReplayAcceptedSend(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.rls-rollback-"+uuid.NewString(),
		"6023"+uuid.NewString()[:8],
		"Rollback after provider acceptance",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Rollback patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			providerCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(
				`{"messages":[{"id":"wamid.accepted-before-crash"}]}`,
			))
		},
	))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	app.Config = &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true},
	}

	simulatedCrash := errors.New("simulated process loss after provider accept")
	firstErr := app.WithTenantApp(
		organization.ID,
		func(scoped *App) error {
			execution := scoped.scopedApp(scoped.DB, organization.ID)
			execution.inboundContinuation = &inboundContinuationExecution{
				MessageID: work.Persisted.ID,
				WAMID:     message.ID,
			}
			execution.ClearContactChatbotTracking(work.Contact.ID)
			_, _, sessionErr := execution.getOrCreateSession(
				organization.ID,
				work.Contact.ID,
				account.Name,
				work.Contact.PhoneNumber,
				30,
			)
			if sessionErr != nil {
				return sessionErr
			}
			if sendErr := execution.sendAndSaveTextMessage(
				account,
				&work.Contact,
				"Exactly one provider attempt",
			); sendErr != nil {
				return sendErr
			}
			return simulatedCrash
		},
	)
	require.ErrorIs(t, firstErr, simulatedCrash)
	assert.Equal(t, int32(1), providerCalls.Load())

	var outgoingCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where(
			"organization_id = ? AND direction = ? AND contact_id = ?",
			organization.ID,
			models.DirectionOutgoing,
			work.Contact.ID,
		).
		Count(&outgoingCount).Error)
	assert.Zero(
		t,
		outgoingCount,
		"the outer tenant transaction was intentionally rolled back",
	)

	replayErr := app.WithTenantApp(
		organization.ID,
		func(scoped *App) error {
			execution := scoped.scopedApp(scoped.DB, organization.ID)
			execution.inboundContinuation = &inboundContinuationExecution{
				MessageID: work.Persisted.ID,
				WAMID:     message.ID,
			}
			execution.ClearContactChatbotTracking(work.Contact.ID)
			_, _, sessionErr := execution.getOrCreateSession(
				organization.ID,
				work.Contact.ID,
				account.Name,
				work.Contact.PhoneNumber,
				30,
			)
			if sessionErr != nil {
				return sessionErr
			}
			return execution.sendAndSaveTextMessage(
				account,
				&work.Contact,
				"Exactly one provider attempt",
			)
		},
	)
	require.Error(t, replayErr)
	assert.True(t, inboundContinuationRequiresManualReview(replayErr))
	assert.Equal(
		t,
		int32(1),
		providerCalls.Load(),
		"the accepted-but-rolled-back send must be reconciled, never replayed",
	)
}

func TestInboundContinuation_UnresolvedPreAttemptIsTerminalBeforeCallback(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.pre-attempt-"+uuid.NewString(),
		"6024"+uuid.NewString()[:8],
		"Crash before callback",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Pre-attempt patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	first := app.scopedApp(app.DB, organization.ID)
	first.inboundContinuation = &inboundContinuationExecution{
		MessageID: work.Persisted.ID,
		WAMID:     message.ID,
	}
	claim, err := first.claimInboundContinuationAction(
		context.Background(),
		"text",
		nil,
	)
	require.NoError(t, err)
	require.True(t, claim.Execute)

	var callbackCalls atomic.Int32
	replay := app.scopedApp(app.DB, organization.ID)
	replay.inboundContinuation = &inboundContinuationExecution{
		MessageID: work.Persisted.ID,
		WAMID:     message.ID,
	}
	err = replay.runInboundContinuationSend(
		"text",
		nil,
		func() (*models.Message, error) {
			callbackCalls.Add(1)
			return nil, errors.New("must not run")
		},
	)
	require.Error(t, err)
	assert.True(t, inboundContinuationRequiresManualReview(err))
	assert.Zero(t, callbackCalls.Load())
}

func TestInboundContinuation_UncertainActionFailsJobAndDuplicateCannotRevive(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	message := inboundContinuationTextMessage(
		t,
		"wamid.manual-review-"+uuid.NewString(),
		"6025"+uuid.NewString()[:8],
		"Reconcile this action",
	)
	work, duplicate, err := app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Manual review patient",
	)
	require.NoError(t, err)
	require.False(t, duplicate)

	baseTime := time.Now().UTC()
	processor := NewInboundContinuationProcessor(app, time.Second)
	processor.now = func() time.Time { return baseTime }
	processCalls := 0
	var providerCallbacks atomic.Int32
	processor.process = func(
		_ context.Context,
		scoped *App,
		_ *persistedIncomingMessage,
	) error {
		processCalls++
		if processCalls == 1 {
			_, claimErr := scoped.claimInboundContinuationAction(
				context.Background(),
				"text",
				nil,
			)
			if claimErr != nil {
				return claimErr
			}
			return errors.New("simulated crash after committed pre-attempt")
		}
		return scoped.runInboundContinuationSend(
			"text",
			nil,
			func() (*models.Message, error) {
				providerCallbacks.Add(1)
				return nil, errors.New("must not dispatch")
			},
		)
	}

	err = processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	)
	require.ErrorContains(t, err, "simulated crash")
	stored := loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusPending, stored.Status)

	processor.now = func() time.Time { return baseTime.Add(2 * time.Second) }
	err = processor.ProcessMessage(
		context.Background(),
		organization.ID,
		work.Persisted.ID,
	)
	require.Error(t, err)
	assert.True(t, inboundContinuationRequiresManualReview(err))
	assert.Zero(t, providerCallbacks.Load())

	stored = loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(t, models.ScheduledJobStatusFailed, stored.Status)
	assert.Equal(t, true, stored.Payload["manual_review_required"])

	_, duplicate, err = app.persistIncomingMessageBeforeAck(
		account.PhoneID,
		message,
		"Manual review patient",
	)
	require.NoError(t, err)
	require.True(t, duplicate)
	stored = loadInboundContinuationJob(
		t,
		app,
		organization.ID,
		work.Persisted.ID,
	)
	assert.Equal(
		t,
		models.ScheduledJobStatusFailed,
		stored.Status,
		"Meta redelivery must not revive a manual-review continuation",
	)
}

func TestInboundContinuation_GraphHTTPActionsReuseDurableNodeResults(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	inboundMessageID := uuid.New()

	t.Run("api mapping is rehydrated without a second request", func(t *testing.T) {
		var apiCalls atomic.Int32
		apiServer := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				apiCalls.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(
					`{"data":{"customer_id":"customer-42","tier":"gold"}}`,
				))
			},
		))
		t.Cleanup(apiServer.Close)
		const apiURL = "https://customer-api.example.com/lookup"
		app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
			"https://customer-api.example.com": apiServer,
		})

		node := &ChatNode{
			ID:   "lookup-customer",
			Type: ChatNodeAPICall,
			Config: map[string]any{
				"url":    apiURL,
				"method": http.MethodPost,
				"headers": map[string]any{
					"Authorization": "Bearer must-not-enter-ledger",
				},
				"body": `{"phone":"{{phone_number}}","secret":"must-not-enter-ledger"}`,
				"response_mapping": map[string]any{
					"customer_id": "data.customer_id",
					"tier":        "data.tier",
				},
			},
		}

		firstSession := &models.ChatbotSession{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organization.ID,
			ContactID:      contact.ID,
			PhoneNumber:    contact.PhoneNumber,
			SessionData:    models.JSONB{},
		}
		first := app.scopedApp(app.DB, organization.ID)
		first.inboundContinuation = &inboundContinuationExecution{
			MessageID: inboundMessageID,
			WAMID:     "wamid.graph-api",
		}
		restore := first.pushInboundContinuationActionScope(
			"chat-graph:flow-a:lookup-customer:visit:0",
		)
		outcome, err := first.execChatAPICallDurable(
			node,
			&chatNodeCtx{
				account: account,
				contact: contact,
				session: firstSession,
			},
		)
		restore()
		require.NoError(t, err)
		assert.Equal(t, "http:2xx", outcome.outcome)
		assert.Equal(t, "customer-42", firstSession.SessionData["customer_id"])
		assert.Equal(t, "gold", firstSession.SessionData["tier"])

		replaySession := &models.ChatbotSession{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organization.ID,
			ContactID:      contact.ID,
			PhoneNumber:    contact.PhoneNumber,
			SessionData:    models.JSONB{},
		}
		replay := app.scopedApp(app.DB, organization.ID)
		replay.inboundContinuation = &inboundContinuationExecution{
			MessageID: inboundMessageID,
			WAMID:     "wamid.graph-api",
		}
		restore = replay.pushInboundContinuationActionScope(
			"chat-graph:flow-a:lookup-customer:visit:0",
		)
		outcome, err = replay.execChatAPICallDurable(
			node,
			&chatNodeCtx{
				account: account,
				contact: contact,
				session: replaySession,
			},
		)
		restore()
		require.NoError(t, err)
		assert.Equal(t, "http:2xx", outcome.outcome)
		assert.Equal(t, "customer-42", replaySession.SessionData["customer_id"])
		assert.Equal(t, "gold", replaySession.SessionData["tier"])
		assert.Equal(t, int32(1), apiCalls.Load())

		var action models.ScheduledJob
		require.NoError(t, app.DB.Where(
			"organization_id = ? AND kind = ? AND aggregate_id = ? AND payload ->> 'effect_kind' = ?",
			organization.ID,
			inboundContinuationActionJobKind,
			inboundMessageID,
			"graph_api_call",
		).First(&action).Error)
		encoded, err := json.Marshal(action.Payload)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), apiURL)
		assert.NotContains(t, string(encoded), "must-not-enter-ledger")
	})

	t.Run("webhook is attempted once per stable node visit", func(t *testing.T) {
		var webhookCalls atomic.Int32
		webhookServer := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				webhookCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			},
		))
		t.Cleanup(webhookServer.Close)
		const webhookURL = "https://crm-webhook.example.com/notify"
		app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
			"https://crm-webhook.example.com": webhookServer,
		})

		node := &ChatNode{
			ID:   "notify-crm",
			Type: ChatNodeWebhook,
			Config: map[string]any{
				"url":    webhookURL,
				"method": http.MethodPost,
				"body":   `{"secret":"webhook-body-must-not-enter-ledger"}`,
			},
		}
		run := func() error {
			session := &models.ChatbotSession{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organization.ID,
				ContactID:      contact.ID,
				PhoneNumber:    contact.PhoneNumber,
				SessionData:    models.JSONB{},
			}
			execution := app.scopedApp(app.DB, organization.ID)
			execution.inboundContinuation = &inboundContinuationExecution{
				MessageID: inboundMessageID,
				WAMID:     "wamid.graph-webhook",
			}
			restore := execution.pushInboundContinuationActionScope(
				"chat-graph:flow-a:notify-crm:visit:1",
			)
			_, err := execution.execChatWebhookDurable(
				node,
				&chatNodeCtx{
					account: account,
					contact: contact,
					session: session,
				},
			)
			restore()
			return err
		}

		require.NoError(t, run())
		require.NoError(t, run())
		assert.Equal(t, int32(1), webhookCalls.Load())

		var action models.ScheduledJob
		require.NoError(t, app.DB.Where(
			"organization_id = ? AND kind = ? AND aggregate_id = ? AND payload ->> 'effect_kind' = ?",
			organization.ID,
			inboundContinuationActionJobKind,
			inboundMessageID,
			"graph_webhook",
		).First(&action).Error)
		encoded, err := json.Marshal(action.Payload)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), webhookURL)
		assert.NotContains(
			t,
			string(encoded),
			"webhook-body-must-not-enter-ledger",
		)
	})
}

func TestMarketingPreference_MergedPhoneAndBSUIDUpdateCanonicalBeforeACK(
	t *testing.T,
) {
	app := newProcessorTestApp(t)
	organization, account := createProcessorTestOrg(t, app)
	canonical := testutil.CreateTestContact(t, app.DB, organization.ID)
	alias := testutil.CreateTestContact(t, app.DB, organization.ID)
	aliasPhone := "6018" + uuid.NewString()[:8]
	aliasBSUID := "bsuid-merged-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, organization.ID).
		Updates(map[string]any{
			"phone_number": aliasPhone,
			"bs_uid":       aliasBSUID,
		}).Error)

	mergedAt := time.Now().UTC()
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", alias.ID, organization.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"merged_at":      mergedAt,
			"deleted_at":     mergedAt,
		}).Error)

	require.NoError(t, app.processMarketingPreference(
		account.PhoneID,
		"+"+aliasPhone,
		"",
		"STOP",
	))

	var storedCanonical models.Contact
	require.NoError(t, app.DB.First(
		&storedCanonical,
		"id = ? AND organization_id = ?",
		canonical.ID,
		organization.ID,
	).Error)
	assert.True(t, storedCanonical.MarketingOptOut)

	var storedAlias models.Contact
	require.NoError(t, app.DB.Unscoped().First(
		&storedAlias,
		"id = ? AND organization_id = ?",
		alias.ID,
		organization.ID,
	).Error)
	assert.False(
		t,
		storedAlias.MarketingOptOut,
		"the preference belongs on the active canonical contact",
	)
	require.NotNil(t, storedAlias.MergedIntoID)
	assert.Equal(t, canonical.ID, *storedAlias.MergedIntoID)

	require.NoError(t, app.processMarketingPreference(
		account.PhoneID,
		"",
		aliasBSUID,
		"RESUME",
	))
	require.NoError(t, app.DB.First(
		&storedCanonical,
		"id = ? AND organization_id = ?",
		canonical.ID,
		organization.ID,
	).Error)
	assert.False(t, storedCanonical.MarketingOptOut)
	assert.Equal(t, aliasBSUID, storedCanonical.BSUID)
}
