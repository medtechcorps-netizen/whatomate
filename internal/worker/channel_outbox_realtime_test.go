package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelOutboxSettlementPublishesCommittedStatus(t *testing.T) {
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()

	t.Run("sent", func(t *testing.T) {
		org := testutil.CreateTestOrganization(t, db)
		workerID := "realtime-sent-" + uuid.NewString()
		account, conversation, message, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
		events := subscribeWorkerRealtime(t, rdb, org.ID)
		worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}

		require.NoError(t, worker.completeChannelOutboxJob(
			org.ID,
			job,
			account,
			workerID,
			channelapi.SendResult{ProviderMessageIDs: []string{"provider-message-1"}},
		))
		event := receiveWorkerRealtime(t, events)
		assert.Equal(t, queue.RealtimeEventMessageStatusChanged, event.Kind)
		assert.Equal(t, string(models.MessageStatusSent), event.Status)
		require.NotNil(t, event.ChannelAccountID)
		assert.Equal(t, account.ID, *event.ChannelAccountID)
		require.NotNil(t, event.ConversationID)
		assert.Equal(t, conversation.ID, *event.ConversationID)
		require.NotNil(t, event.MessageID)
		assert.Equal(t, message.ID, *event.MessageID)
	})

	t.Run("retrying", func(t *testing.T) {
		org := testutil.CreateTestOrganization(t, db)
		workerID := "realtime-retry-" + uuid.NewString()
		_, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
		events := subscribeWorkerRealtime(t, rdb, org.ID)
		worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}

		require.NoError(t, worker.failChannelOutboxJob(
			org.ID,
			job,
			workerID,
			errors.New("temporary provider failure"),
			true,
		))
		event := receiveWorkerRealtime(t, events)
		assert.Equal(t, string(models.OutboxJobStatusRetrying), event.Status)
	})

	t.Run("terminal failed", func(t *testing.T) {
		org := testutil.CreateTestOrganization(t, db)
		workerID := "realtime-failed-" + uuid.NewString()
		_, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
		events := subscribeWorkerRealtime(t, rdb, org.ID)
		worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}
		require.NoError(t, worker.failChannelOutboxJob(
			org.ID, job, workerID, errors.New("terminal provider failure"), false,
		))
		event := receiveWorkerRealtime(t, events)
		assert.Equal(t, string(models.OutboxJobStatusFailed), event.Status)
	})

	t.Run("AI cancelled", func(t *testing.T) {
		org := testutil.CreateTestOrganization(t, db)
		workerID := "realtime-cancelled-" + uuid.NewString()
		_, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
		events := subscribeWorkerRealtime(t, rdb, org.ID)
		worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}
		require.NoError(t, worker.cancelChannelAIOutboxJob(
			org.ID, job, workerID, errors.New("human reply won"),
		))
		event := receiveWorkerRealtime(t, events)
		assert.Equal(t, string(models.OutboxJobStatusCancelled), event.Status)
	})

	t.Run("interrupted dispatch recovery", func(t *testing.T) {
		org := testutil.CreateTestOrganization(t, db)
		workerID := "realtime-recovery-" + uuid.NewString()
		_, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
		stale := time.Now().UTC().Add(-2 * defaultChannelOutboxLease)
		require.NoError(t, db.Model(&models.OutboxJob{}).Where("id = ?", job.ID).Updates(map[string]any{
			"status": models.OutboxJobStatusDispatching, "locked_at": stale, "available_at": stale,
		}).Error)
		events := subscribeWorkerRealtime(t, rdb, org.ID)
		worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}
		claimedID, claimed, err := worker.claimChannelOutboxJob(org.ID, workerID+"-new")
		require.NoError(t, err)
		assert.False(t, claimed)
		assert.Equal(t, uuid.Nil, claimedID)
		event := receiveWorkerRealtime(t, events)
		assert.Equal(t, string(models.OutboxJobStatusFailed), event.Status)
	})
}

func TestChannelOutboxSettlementFailurePublishesNothing(t *testing.T) {
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	org := testutil.CreateTestOrganization(t, db)
	workerID := "realtime-rollback-" + uuid.NewString()
	account, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
	events := subscribeWorkerRealtime(t, rdb, org.ID)
	callbackName := "test:fail_channel_outbox_settlement_" + uuid.NewString()
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "message_events" {
			tx.AddError(errors.New("forced message-event persistence failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(rdb, log)}
	require.Error(t, worker.completeChannelOutboxJob(
		org.ID,
		job,
		account,
		workerID,
		channelapi.SendResult{ProviderMessageIDs: []string{"must-roll-back"}},
	))
	select {
	case event := <-events:
		t.Fatalf("rolled-back settlement emitted realtime event: %#v", event)
	case <-time.After(200 * time.Millisecond):
	}
	var persisted models.OutboxJob
	require.NoError(t, db.First(&persisted, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, persisted.Status)
}

func TestChannelOutboxSettlementSurvivesUnavailableRealtimePublisher(t *testing.T) {
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	org := testutil.CreateTestOrganization(t, db)
	workerID := "realtime-unavailable-" + uuid.NewString()
	account, _, message, job := createChannelOutboxTestFixture(t, db, org.ID, workerID)
	worker := &Worker{DB: db, Log: log, Publisher: queue.NewPublisher(nil, log)}

	require.NoError(t, worker.completeChannelOutboxJob(
		org.ID,
		job,
		account,
		workerID,
		channelapi.SendResult{ProviderMessageIDs: []string{"provider-message-2"}},
	))
	var persisted models.Message
	require.NoError(t, db.First(&persisted, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, persisted.Status)
}

func subscribeWorkerRealtime(t *testing.T, rdb *redis.Client, organizationID uuid.UUID) <-chan queue.RealtimeEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	subscriber := queue.NewSubscriber(rdb, testutil.NopLogger())
	t.Cleanup(func() {
		cancel()
		_ = subscriber.Close()
	})
	events := make(chan queue.RealtimeEvent, 4)
	require.NoError(t, subscriber.SubscribeRealtime(ctx, func(event *queue.RealtimeEvent) {
		if event != nil && event.OrganizationID == organizationID {
			events <- *event
		}
	}))
	return events
}

func receiveWorkerRealtime(t *testing.T, events <-chan queue.RealtimeEvent) queue.RealtimeEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for worker realtime event")
		return queue.RealtimeEvent{}
	}
}
