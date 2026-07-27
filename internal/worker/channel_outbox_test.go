package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelOutboxBackoffIsBounded(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2*time.Second, channelOutboxBackoff(1))
	assert.Equal(t, 4*time.Second, channelOutboxBackoff(2))
	assert.Equal(t, time.Hour, channelOutboxBackoff(20))
}

func TestRetryableChannelErrorUsesProviderClassification(t *testing.T) {
	t.Parallel()

	assert.True(t, retryableChannelError(errors.New("network failure")))
	assert.True(t, retryableChannelError(&channelapi.ProviderError{
		Message:   "rate limited",
		Retryable: true,
	}))
	assert.False(t, retryableChannelError(&channelapi.ProviderError{
		Message:   "invalid recipient",
		Retryable: false,
	}))
}

func TestDecodeChannelOutboxPayload(t *testing.T) {
	t.Parallel()

	payload := models.JSONB{
		"organization_id": "b26b1e34-c408-4eb8-bf09-6fc92e04404d",
		"idempotency_key": "message-1",
		"conversation": map[string]any{
			"external_id": "thread-1",
		},
		"parts": []any{
			map[string]any{"type": "text", "text": "Hello"},
		},
	}
	var message channelapi.OutboundMessage
	require.NoError(t, decodeChannelOutboxPayload(payload, &message))
	assert.Equal(t, "message-1", message.IdempotencyKey)
	require.Len(t, message.Parts, 1)
	assert.Equal(t, models.MessagePartTypeText, message.Parts[0].Type)
	assert.Equal(t, "Hello", message.Parts[0].Text)
}

func TestChannelOutboxErrorMessageIsBounded(t *testing.T) {
	t.Parallel()

	message := make([]byte, 2000)
	for i := range message {
		message[i] = 'x'
	}
	assert.Len(t, channelOutboxErrorMessage(errors.New(string(message))), 1000)
}

func TestChannelOutboundMessageUsesPersistedIdempotencyKey(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	messageID := uuid.New()
	job := &models.OutboxJob{
		OrganizationID: orgID,
		MessageID:      &messageID,
		IdempotencyKey: "persisted-key",
		Payload: models.JSONB{
			"organization_id": "bd6ff6ca-e0ed-442d-b206-3a1ed7b80ccd",
			"message_id":      "23ce3f32-8e89-4e59-a805-b6cc35d7e7bf",
			"idempotency_key": "payload-key-must-not-win",
			"parts": []any{
				map[string]any{"type": "text", "text": "Hello"},
			},
		},
	}
	outbound, err := channelOutboundMessageForJob(job)
	require.NoError(t, err)
	assert.Equal(t, orgID, outbound.OrganizationID)
	assert.Equal(t, messageID, outbound.MessageID)
	assert.Equal(t, "persisted-key", outbound.IdempotencyKey)
}

func TestChannelOutboxRetryThenDeadLettersAtMaxAttempts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelWebChat,
		Provider:          channelapi.RelayProvider,
		Name:              "outbox-" + uuid.NewString()[:8],
		ExternalAccountID: "site-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		PhoneNumber:     "external-" + uuid.NewString(),
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(contact).Error)
	conversation := &models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         org.ID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		Channel:                account.Channel,
		ExternalConversationID: "thread-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(conversation).Error)
	message := &models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionOutgoing,
		MessageType:         models.MessageTypeText,
		Content:             "Hello",
		Status:              models.MessageStatusPending,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	workerID := "test-worker"
	job := &models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		ConversationID:   conversation.ID,
		MessageID:        &message.ID,
		IdempotencyKey:   "retry-" + uuid.NewString(),
		Status:           models.OutboxJobStatusProcessing,
		AvailableAt:      time.Now().UTC(),
		LockedAt:         timePointerForWorkerTest(time.Now().UTC()),
		LockedBy:         workerID,
		AttemptCount:     0,
		MaxAttempts:      2,
		Payload:          models.JSONB{},
	}
	require.NoError(t, db.Create(job).Error)
	w := &Worker{DB: db, Log: testutil.NopLogger()}

	require.NoError(t, w.failChannelOutboxJob(org.ID, job, workerID, errors.New("temporary"), true))
	require.NoError(t, db.Where("id = ? AND organization_id = ?", job.ID, org.ID).First(job).Error)
	assert.Equal(t, models.OutboxJobStatusRetrying, job.Status)
	assert.Equal(t, 1, job.AttemptCount)
	assert.True(t, job.AvailableAt.After(time.Now().UTC()))

	now := time.Now().UTC()
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", job.ID, org.ID).
		Updates(map[string]any{
			"status":    models.OutboxJobStatusProcessing,
			"locked_at": now,
			"locked_by": workerID,
		}).Error)
	job.Status = models.OutboxJobStatusProcessing
	job.LockedAt = &now
	job.LockedBy = workerID
	require.NoError(t, w.failChannelOutboxJob(org.ID, job, workerID, errors.New("still failing"), true))

	require.NoError(t, db.Where("id = ? AND organization_id = ?", job.ID, org.ID).First(job).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Equal(t, 2, job.AttemptCount)
	require.NotNil(t, job.FailedAt)

	require.NoError(t, db.Where("id = ? AND organization_id = ?", message.ID, org.ID).First(message).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)
	var failedEvents int64
	require.NoError(t, db.Model(&models.MessageEvent{}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND message_id = ? AND type = ?",
			org.ID,
			account.ID,
			message.ID,
			models.MessageEventTypeFailed,
		).
		Count(&failedEvents).Error)
	assert.EqualValues(t, 1, failedEvents)

	duplicate := *job
	duplicate.BaseModel = models.BaseModel{ID: uuid.New()}
	duplicate.Status = models.OutboxJobStatusPending
	duplicate.MessageID = nil
	duplicate.LockedAt = nil
	duplicate.LockedBy = ""
	require.Error(t, db.Create(&duplicate).Error, "tenant/account idempotency key must be unique")
}

func TestChannelOutboxUnlicensedTenantIsDeadLettered(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, conversation, message, job := createChannelOutboxTestFixture(t, db, org.ID, "unlicensed-worker")
	worker := &Worker{DB: db, Log: testutil.NopLogger()}

	require.NoError(t, worker.deliverChannelOutboxJob(
		context.Background(),
		org.ID,
		job.ID,
		job.LockedBy,
	))
	require.NoError(t, db.Where("id = ? AND organization_id = ?", job.ID, org.ID).First(job).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Contains(t, job.LastError, "entitlement")
	require.NoError(t, db.Where("id = ? AND organization_id = ?", message.ID, org.ID).First(message).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)

	_ = account
	_ = conversation
}

func TestChannelOutboxReclaimsStaleLeaseAndProtectsCompletion(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, message, job := createChannelOutboxTestFixture(t, db, org.ID, "old-worker")
	stale := time.Now().UTC().Add(-defaultChannelOutboxLease - time.Second)
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", job.ID, org.ID).
		Updates(map[string]any{"locked_at": stale}).Error)

	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	claimedID, claimed, err := worker.claimChannelOutboxJob(org.ID, "replacement-worker")
	require.NoError(t, err)
	assert.True(t, claimed)
	assert.Equal(t, job.ID, claimedID)

	err = worker.completeChannelOutboxJob(
		org.ID,
		job,
		account,
		"old-worker",
		channelapi.SendResult{ProviderMessageIDs: []string{"provider-message"}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lost its lease")

	job.LockedBy = "replacement-worker"
	require.NoError(t, worker.completeChannelOutboxJob(
		org.ID,
		job,
		account,
		"replacement-worker",
		channelapi.SendResult{ProviderMessageIDs: []string{"provider-message"}},
	))
	require.NoError(t, db.Where("id = ? AND organization_id = ?", job.ID, org.ID).First(job).Error)
	assert.Equal(t, models.OutboxJobStatusSent, job.Status)
	require.NoError(t, db.Where("id = ? AND organization_id = ?", message.ID, org.ID).First(message).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
}

func createChannelOutboxTestFixture(
	t *testing.T,
	db *gorm.DB,
	orgID uuid.UUID,
	workerID string,
) (*models.ChannelAccount, *models.InboxConversation, *models.Message, *models.OutboxJob) {
	t.Helper()

	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Channel:           models.ChannelWebChat,
		Provider:          channelapi.RelayProvider,
		Name:              "fixture-" + uuid.NewString()[:8],
		ExternalAccountID: "site-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		PhoneNumber:     "external-" + uuid.NewString(),
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(contact).Error)
	conversation := &models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         orgID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		Channel:                account.Channel,
		ExternalConversationID: "thread-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(conversation).Error)
	message := &models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      orgID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionOutgoing,
		MessageType:         models.MessageTypeText,
		Content:             "Hello",
		Status:              models.MessageStatusPending,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	now := time.Now().UTC()
	job := &models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: account.ID,
		ConversationID:   conversation.ID,
		MessageID:        &message.ID,
		IdempotencyKey:   "fixture-" + uuid.NewString(),
		PayloadDigest:    strings.Repeat("a", 64),
		Purpose:          models.ChannelPreferencePurposeService,
		Status:           models.OutboxJobStatusProcessing,
		AvailableAt:      now,
		LockedAt:         &now,
		LockedBy:         workerID,
		MaxAttempts:      2,
		Payload: models.JSONB{
			"purpose": models.ChannelPreferencePurposeService,
			"parts": []any{
				map[string]any{"type": "text", "text": "Hello"},
			},
		},
	}
	require.NoError(t, db.Create(job).Error)
	return account, conversation, message, job
}

func timePointerForWorkerTest(value time.Time) *time.Time {
	return &value
}
