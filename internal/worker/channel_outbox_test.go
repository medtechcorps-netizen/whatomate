package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestChannelOutboxBackoffIsBounded(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2*time.Second, channelOutboxBackoff(1))
	assert.Equal(t, 4*time.Second, channelOutboxBackoff(2))
	assert.Equal(t, time.Hour, channelOutboxBackoff(20))
}

func TestChannelOutboxDurableEntitlementSemantics(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	assert.False(t, channelOutboxSubscriptionPermitsEntitlement(
		&models.Subscription{
			Status:           models.SubscriptionStatusActive,
			CurrentPeriodEnd: &expired,
		},
		now,
	))
	assert.True(t, channelOutboxSubscriptionPermitsEntitlement(
		&models.Subscription{
			Status:           models.SubscriptionStatusActive,
			CurrentPeriodEnd: &future,
		},
		now,
	))
	assert.True(t, channelOutboxEntitlementAllows(models.JSONB{"enabled": true}))
	assert.False(t, channelOutboxEntitlementAllows("disabled"))
	assert.False(t, channelOutboxEntitlementAllows(nil))
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

func TestListReadyChannelOutboxOrganizationsWithoutRLS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	_, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, "ready-org-worker")
	readyAt := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", job.ID, org.ID).
		Updates(map[string]any{
			"status":       models.OutboxJobStatusPending,
			"available_at": readyAt,
			"locked_at":    nil,
			"locked_by":    "",
		}).Error)

	w := &Worker{DB: db, Log: testutil.NopLogger()}
	organizationIDs, err := w.listReadyChannelOutboxOrganizations(
		defaultChannelOutboxBatchSize,
		time.Now().UTC(),
		time.Now().UTC().Add(-defaultChannelOutboxLease),
	)
	require.NoError(t, err)
	assert.Contains(t, organizationIDs, org.ID)
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

func TestChannelOutboxThreadsEntitlementIsRechecked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, message, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"threads-entitlement-worker",
	)
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Updates(map[string]any{
			"channel":  models.ChannelThreads,
			"provider": channelapi.ThreadsProvider,
		}).Error)
	enableChannelAIReplyOmnichannel(t, db, org.ID)

	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, worker.deliverChannelOutboxJob(
		context.Background(),
		org.ID,
		job.ID,
		job.LockedBy,
	))
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Contains(t, job.LastError, "threads public engagement entitlement")
	require.NoError(t, db.First(message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)
}

func TestChannelOutboxRejectsMismatchedThreadsAppBinding(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, message, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"threads-binding-worker",
	)
	const configuredAppID = "1234567890123456"
	const staleAppID = "9999999999999999"
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Updates(map[string]any{
			"channel":  models.ChannelThreads,
			"provider": channelapi.ThreadsProvider,
			"metadata": models.JSONB{"app_id": staleAppID},
		}).Error)
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", org.ID, account.ID).
		Update("metadata", models.JSONB{"app_id": configuredAppID}).Error)
	appID := configuredAppID
	require.NoError(t, db.Create(&models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Provider:       channelapi.ThreadsProvider,
		ThreadsAppID:   &appID,
		Enabled:        true,
		Config:         models.JSONB{"app_id": configuredAppID},
		CredentialData: models.JSONB{},
	}).Error)
	enableChannelAIReplyOmnichannel(t, db, org.ID)
	var subscription models.Subscription
	require.NoError(t, db.Where("organization_id = ?", org.ID).First(&subscription).Error)
	subscription.EntitlementsSnapshot[channelapi.ThreadsPublicEngagementEntitlementKey] = true
	require.NoError(t, db.Model(&subscription).
		Update("entitlements_snapshot", subscription.EntitlementsSnapshot).Error)

	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, worker.deliverChannelOutboxJob(
		context.Background(),
		org.ID,
		job.ID,
		job.LockedBy,
	))
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Contains(t, job.LastError, "threads app binding")
	require.NoError(t, db.First(message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)
}

func TestPreparedChannelOutboxPersistsStateBeforeSinglePublishAttempt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, message, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"prepared-publish-worker",
	)
	providerState := models.JSONB{"container_id": "container-123"}
	sender := &preparedChannelOutboxTestSender{
		prepareState: providerState,
		publish: func(
			_ context.Context,
			_ *models.ChannelAccount,
			_ channelapi.OutboundMessage,
			state models.JSONB,
		) (channelapi.SendResult, error) {
			var persisted models.OutboxJob
			require.NoError(t, db.First(&persisted, "id = ?", job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusDispatching, persisted.Status)
			assert.Equal(t, providerState, persisted.ProviderState)
			assert.Equal(t, providerState, state)
			return channelapi.SendResult{}, &channelapi.ProviderError{
				Operation: "publish_prepared",
				Provider:  channelapi.ThreadsProvider,
				Code:      "upstream_timeout",
				Message:   "publish response was not received",
				Retryable: true,
			}
		},
	}
	worker := &Worker{DB: db, Log: testutil.NopLogger()}

	require.NoError(t, worker.deliverPreparedChannelOutboxJob(
		context.Background(),
		org.ID,
		job,
		account,
		job.LockedBy,
		channelapi.OutboundMessage{},
		sender,
	))
	assert.Equal(t, 1, sender.prepareCalls)
	assert.Equal(t, 1, sender.publishCalls)

	var persisted models.OutboxJob
	require.NoError(t, db.First(&persisted, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, persisted.Status)
	assert.Equal(t, 1, persisted.AttemptCount)
	assert.Equal(t, providerState, persisted.ProviderState)
	require.NoError(t, db.First(message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)

	// The terminal row cannot cross the dispatch fence again, even if a caller
	// accidentally attempts to process the same in-memory job a second time.
	require.Error(t, worker.deliverPreparedChannelOutboxJob(
		context.Background(),
		org.ID,
		job,
		account,
		job.LockedBy,
		channelapi.OutboundMessage{},
		sender,
	))
	assert.Equal(t, 1, sender.prepareCalls)
	assert.Equal(t, 1, sender.publishCalls)
}

func TestPreparedChannelOutboxResumesPersistedPreparation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, message, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"prepared-resume-worker",
	)
	providerState := models.JSONB{"container_id": "container-resumed"}
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", job.ID, org.ID).
		Update("provider_state", providerState).Error)
	job.ProviderState = providerState
	sender := &preparedChannelOutboxTestSender{
		prepareState: models.JSONB{"container_id": "must-not-be-created"},
		publish: func(
			_ context.Context,
			_ *models.ChannelAccount,
			_ channelapi.OutboundMessage,
			state models.JSONB,
		) (channelapi.SendResult, error) {
			assert.Equal(t, providerState, state)
			return channelapi.SendResult{
				ProviderMessageIDs: []string{"threads-message-123"},
			}, nil
		},
	}
	worker := &Worker{DB: db, Log: testutil.NopLogger()}

	require.NoError(t, worker.deliverPreparedChannelOutboxJob(
		context.Background(),
		org.ID,
		job,
		account,
		job.LockedBy,
		channelapi.OutboundMessage{},
		sender,
	))
	assert.Zero(t, sender.prepareCalls)
	assert.Equal(t, 1, sender.publishCalls)
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusSent, job.Status)
	require.NoError(t, db.First(message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusSent, message.Status)
}

func TestThreadsDispatchFenceWaitsForDisconnectAndDoesNotPublish(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, _, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"threads-disconnect-fence-worker",
	)
	configureThreadsDispatchFenceFixture(t, db, org.ID, account, job, "current-token")

	disconnectTx := db.Begin()
	require.NoError(t, disconnectTx.Error)
	t.Cleanup(func() { _ = disconnectTx.Rollback().Error })
	var disconnecting models.ChannelAccount
	require.NoError(t, disconnectTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		First(&disconnecting).Error)
	disconnecting.Config = cloneChannelOutboxTestJSONB(disconnecting.Config)
	disconnecting.Config["outbound_enabled"] = false
	disconnecting.Status = models.ChannelAccountStatusDisconnected
	require.NoError(t, disconnectTx.Save(&disconnecting).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			worker := &Worker{DB: connection, Log: testutil.NopLogger()}
			_, err := worker.markChannelOutboxDispatching(
				org.ID,
				job.ID,
				account.ID,
				job.LockedBy,
			)
			return err
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	now := time.Now().UTC()
	require.NoError(t, disconnectTx.Model(&models.OutboxJob{}).
		Where(
			"id = ? AND organization_id = ? AND status = ?",
			job.ID,
			org.ID,
			models.OutboxJobStatusProcessing,
		).
		Updates(map[string]any{
			"status":     models.OutboxJobStatusCancelled,
			"failed_at":  now,
			"locked_at":  nil,
			"locked_by":  "",
			"updated_at": now,
		}).Error)
	require.NoError(t, disconnectTx.Commit().Error)

	select {
	case err := <-fenceDone:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer active")
	case <-time.After(5 * time.Second):
		require.Fail(t, "Threads dispatch fence did not resume after disconnect committed")
	}
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, job.Status)
}

func TestThreadsDispatchFencePublishesWithCurrentCredentialSnapshot(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, _, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"threads-current-credential-worker",
	)
	credential := configureThreadsDispatchFenceFixture(
		t,
		db,
		org.ID,
		account,
		job,
		"current-token",
	)
	account.Credentials = []models.ChannelCredential{{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		CredentialBlob: models.JSONB{"access_token": "stale-in-memory-token"},
		Status:         models.ChannelCredentialStatusRevoked,
	}}
	sender := &preparedChannelOutboxTestSender{
		prepareState: models.JSONB{"container_id": "must-not-be-created"},
		publish: func(
			_ context.Context,
			fenced *models.ChannelAccount,
			_ channelapi.OutboundMessage,
			_ models.JSONB,
		) (channelapi.SendResult, error) {
			require.Len(t, fenced.Credentials, 1)
			assert.Equal(t, credential.ID, fenced.Credentials[0].ID)
			assert.Equal(
				t,
				"current-token",
				fenced.Credentials[0].CredentialBlob["access_token"],
			)
			return channelapi.SendResult{
				ProviderMessageIDs: []string{"threads-current-credential-message"},
			}, nil
		},
	}
	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, worker.deliverPreparedChannelOutboxJob(
		context.Background(),
		org.ID,
		job,
		account,
		job.LockedBy,
		channelapi.OutboundMessage{},
		sender,
	))
	assert.Zero(t, sender.prepareCalls)
	assert.Equal(t, 1, sender.publishCalls)
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusSent, job.Status)
}

func TestChannelOutboxResolvesThreadsAdapter(t *testing.T) {
	worker := &Worker{
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: "test-threads-encryption-key"},
		},
	}
	adapter, err := worker.channelOutboxAdapter(&models.ChannelAccount{
		Channel:  models.ChannelThreads,
		Provider: channelapi.ThreadsProvider,
	})
	require.NoError(t, err)
	assert.Equal(t, models.ChannelThreads, adapter.Channel())
	assert.Equal(t, channelapi.ThreadsProvider, adapter.Provider())
	assert.Implements(t, (*channelapi.PreparedSender)(nil), adapter)
}

func TestChannelOutboxMetaRelayRequiresProtectedBinding(t *testing.T) {
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.MustParse("00000000-0000-4000-8000-000000000001")},
		OrganizationID:    uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"),
		Channel:           models.ChannelMessenger,
		Provider:          channelapi.RelayProvider,
		ExternalAccountID: "700000000000099",
		Config: models.JSONB{
			"relay_url":             "https://app.rereply.app/meta-relay/v1/accounts/messenger/700000000000099",
			"identity_confirmed_id": "700000000000099",
			"outbound_enabled":      true,
		},
		Status:            models.ChannelAccountStatusActive,
		LastHealthCheckAt: timePointerForWorkerTest(time.Now().UTC().Add(-time.Minute)),
		LastInboundAt:     timePointerForWorkerTest(time.Now().UTC()),
		Metadata: models.JSONB{
			channelapi.MetaProviderProofMetadataKey: channelapi.MetaProviderProofVersion,
		},
		Credentials: []models.ChannelCredential{{
			Kind:   models.ChannelCredentialKindWebhook,
			Status: models.ChannelCredentialStatusActive,
		}},
	}
	worker := &Worker{Config: &config.Config{
		App: config.AppConfig{Environment: "production", EncryptionKey: "test-meta-relay-key"},
		MetaRelay: config.MetaRelayConfig{
			BaseURL:             "https://app.rereply.app/meta-relay",
			ProviderProofSecret: "worker-meta-provider-proof-secret-at-least-32-bytes",
			ExpectedAccountsJSON: `{"accounts":[{
                "organization_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
                "meta_business_id":"300000000000099",
                "channel":"messenger",
                "external_account_id":"700000000000099",
                "rereply_account_id":"00000000-0000-4000-8000-000000000001"
            }]}`,
		},
	}}

	adapter, err := worker.channelOutboxAdapter(account)
	require.NoError(t, err)
	assert.Equal(t, models.ChannelMessenger, adapter.Channel())

	account.Config["relay_url"] = "https://attacker.example/v1/accounts/messenger/700000000000099"
	_, err = worker.channelOutboxAdapter(account)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trusted Meta relay binding")
}

func TestChannelOutboxMetaSendGateRejectsLegacyEvidenceWithoutProofMarker(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	accountID := uuid.New()
	externalID := "700000000000099"
	healthAt := time.Now().UTC().Add(-time.Minute)
	inboundAt := healthAt.Add(time.Second)
	relayURL := "https://app.rereply.app/meta-relay/v1/accounts/messenger/" + externalID
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: accountID},
		OrganizationID:    org.ID,
		Channel:           models.ChannelMessenger,
		Provider:          channelapi.RelayProvider,
		Name:              "legacy-enabled-meta",
		ExternalAccountID: externalID,
		Status:            models.ChannelAccountStatusActive,
		Config: models.JSONB{
			"relay_url":             relayURL,
			"identity_confirmed_id": externalID,
			"outbound_enabled":      true,
		},
		Metadata:          models.JSONB{},
		LastHealthCheckAt: &healthAt,
		LastInboundAt:     &inboundAt,
	}
	require.NoError(t, db.Create(account).Error)
	require.NoError(t, db.Create(&models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          1,
		CredentialBlob:   models.JSONB{"inbound_secret": "tenant-secret"},
		Status:           models.ChannelCredentialStatusActive,
		Metadata:         models.JSONB{},
	}).Error)
	worker := &Worker{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{
				Environment:   "production",
				EncryptionKey: "worker-test-encryption-key",
			},
			MetaRelay: config.MetaRelayConfig{
				BaseURL:             "https://app.rereply.app/meta-relay",
				ProviderProofSecret: "worker-meta-provider-proof-secret-at-least-32-bytes",
				ExpectedAccountsJSON: fmt.Sprintf(`{"accounts":[{
					"organization_id":%q,
					"meta_business_id":"300000000000099",
					"channel":"messenger",
					"external_account_id":%q,
					"rereply_account_id":%q
				}]}`, org.ID.String(), externalID, account.ID.String()),
			},
		},
	}

	_, err := worker.metaRelayOutboundAccountAtSend(org.ID, account.ID)
	require.ErrorContains(t, err, "current provider-proof Test")

	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Update("metadata", models.JSONB{
			channelapi.MetaProviderProofMetadataKey: channelapi.MetaProviderProofVersion,
		}).Error)
	verified, err := worker.metaRelayOutboundAccountAtSend(org.ID, account.ID)
	require.NoError(t, err)
	assert.Equal(t, channelapi.MetaProviderProofVersion, verified.Metadata[channelapi.MetaProviderProofMetadataKey])
}

func TestMetaRelayDispatchFenceWaitsForDisconnectAndDoesNotCross(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, _, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"meta-disconnect-fence-worker",
	)
	worker := configureMetaRelayDispatchFenceFixture(t, db, org.ID, account)

	disconnectTx := db.Begin()
	require.NoError(t, disconnectTx.Error)
	t.Cleanup(func() { _ = disconnectTx.Rollback().Error })
	var disconnecting models.ChannelAccount
	require.NoError(t, disconnectTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		First(&disconnecting).Error)
	disconnecting.Config = cloneChannelOutboxTestJSONB(disconnecting.Config)
	disconnecting.Config["outbound_enabled"] = false
	disconnecting.Status = models.ChannelAccountStatusDisconnected
	require.NoError(t, disconnectTx.Save(&disconnecting).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			fencedWorker := *worker
			fencedWorker.DB = connection
			_, err := fencedWorker.markMetaRelayOutboxDispatching(
				org.ID,
				job.ID,
				account.ID,
				job.LockedBy,
			)
			return err
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	now := time.Now().UTC()
	require.NoError(t, disconnectTx.Model(&models.OutboxJob{}).
		Where(
			"id = ? AND organization_id = ? AND status = ?",
			job.ID,
			org.ID,
			models.OutboxJobStatusProcessing,
		).
		Updates(map[string]any{
			"status":     models.OutboxJobStatusCancelled,
			"failed_at":  now,
			"locked_at":  nil,
			"locked_by":  "",
			"updated_at": now,
		}).Error)
	require.NoError(t, disconnectTx.Commit().Error)

	select {
	case err := <-fenceDone:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "successful current health check")
	case <-time.After(5 * time.Second):
		require.Fail(t, "Meta relay dispatch fence did not resume after disconnect committed")
	}
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, job.Status)
}

func TestMetaRelayDispatchFenceAllowsOAuthAlongsideOneWebhookCredential(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, _, job := createChannelOutboxTestFixture(
		t,
		db,
		org.ID,
		"meta-current-credential-worker",
	)
	worker := configureMetaRelayDispatchFenceFixture(t, db, org.ID, account)
	require.NoError(t, db.Create(&models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": "encrypted-oauth-token"},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test:v1",
		Metadata:         models.JSONB{},
	}).Error)

	fenced, err := worker.markMetaRelayOutboxDispatching(
		org.ID,
		job.ID,
		account.ID,
		job.LockedBy,
	)
	require.NoError(t, err)
	require.Len(t, fenced.Credentials, 2)
	assert.Equal(t, 1, channelapi.CurrentCredentialCountOfKind(
		fenced.Credentials,
		models.ChannelCredentialKindWebhook,
		time.Now().UTC(),
	))
	assert.Equal(t, 1, channelapi.CurrentCredentialCountOfKind(
		fenced.Credentials,
		models.ChannelCredentialKindOAuth,
		time.Now().UTC(),
	))

	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, job.Status)
}

func TestChannelOutboxAdapterFailsClosedWithoutWorkerConfig(t *testing.T) {
	worker := &Worker{}
	_, err := worker.channelOutboxAdapter(&models.ChannelAccount{
		Channel:  models.ChannelMessenger,
		Provider: channelapi.RelayProvider,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "application configuration is unavailable")
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

func TestChannelAIOutboxDispatchRechecksPolicyAndCancels(t *testing.T) {
	tests := []struct {
		name         string
		frozenWindow func() time.Time
		changePolicy func(*testing.T, *gorm.DB, *channelAIReplyFixture)
	}{
		{
			name: "conversation paused",
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				require.NoError(t, db.Model(&models.InboxConversation{}).
					Where("id = ?", fixture.Conversation.ID).
					Update(
						"config",
						models.JSONB{models.ConversationConfigAIPaused: true},
					).Error)
			},
		},
		{
			name: "account AI disabled",
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				require.NoError(t, db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.Account.ID).
					Update("config", models.JSONB{
						"outbound_enabled": true,
						"ai_reply_enabled": false,
					}).Error)
			},
		},
		{
			name: "qwen settings disabled",
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				require.NoError(t, db.Model(&models.ChatbotSettings{}).
					Where("id = ?", fixture.Settings.ID).
					Update("ai_enabled", false).Error)
			},
		},
		{
			name: "newer human reply",
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				createChannelAIReplyHumanMessage(t, db, fixture)
			},
		},
		{
			name: "active human handover",
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				require.NoError(t, db.Create(&models.AgentTransfer{
					BaseModel:       models.BaseModel{ID: uuid.New()},
					OrganizationID:  fixture.Organization.ID,
					ContactID:       fixture.Contact.ID,
					WhatsAppAccount: fixture.Account.Name,
					PhoneNumber:     fixture.Contact.PhoneNumber,
					Status:          models.TransferStatusActive,
					Source:          models.TransferSourceManual,
					TransferredAt:   time.Now().UTC(),
				}).Error)
			},
		},
		{
			name: "frozen service window expired despite a reopened conversation",
			frozenWindow: func() time.Time {
				return time.Now().UTC().Add(-time.Minute)
			},
			changePolicy: func(
				t *testing.T,
				db *gorm.DB,
				fixture *channelAIReplyFixture,
			) {
				reopened := time.Now().UTC().Add(time.Hour)
				require.NoError(t, db.Model(&models.InboxConversation{}).
					Where("id = ?", fixture.Conversation.ID).
					Update("service_window_ends_at", reopened).Error)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			fixture := createChannelAIReplyWorkerFixture(t, db)
			windowEnd := time.Now().UTC().Add(time.Hour)
			if test.frozenWindow != nil {
				windowEnd = test.frozenWindow()
			}
			job, message := createChannelAIOutboxDispatchFixture(
				t,
				db,
				fixture,
				windowEnd,
				"ai-dispatch-worker",
			)
			test.changePolicy(t, db, fixture)

			worker := &Worker{DB: db, Log: testutil.NopLogger()}
			err := worker.recheckChannelAIOutboxDispatch(
				fixture.Organization.ID,
				job.ID,
				job.LockedBy,
			)
			require.ErrorIs(t, err, errChannelOutboxAIPolicy)
			require.NoError(t, worker.cancelChannelAIOutboxJob(
				fixture.Organization.ID,
				job,
				job.LockedBy,
				err,
			))

			require.NoError(t, db.First(job, "id = ?", job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusCancelled, job.Status)
			assert.Equal(t, "ai_reply_cancelled", job.LastErrorCode)
			require.NoError(t, db.First(message, "id = ?", message.ID).Error)
			assert.Equal(t, models.MessageStatusFailed, message.Status)
		})
	}
}

func TestChannelAIOutboxDispatchAllowsUnchangedEligibleJob(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"ai-dispatch-worker",
	)

	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	require.NoError(t, worker.recheckChannelAIOutboxDispatch(
		fixture.Organization.ID,
		job.ID,
		job.LockedBy,
	))
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, job.Status)

	// Once the atomic fence wins, a later control-plane cancellation cannot
	// revoke the already-authorized single provider attempt.
	result := db.Model(&models.OutboxJob{}).
		Where(
			"id = ? AND status IN ?",
			job.ID,
			[]models.OutboxJobStatus{
				models.OutboxJobStatusPending,
				models.OutboxJobStatusRetrying,
				models.OutboxJobStatusProcessing,
			},
		).
		Update("status", models.OutboxJobStatusCancelled)
	require.NoError(t, result.Error)
	assert.Zero(t, result.RowsAffected)
}

func TestChannelAIOutboxDispatchSerializesCommittedConsentWithdrawal(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"consent-race-worker",
	)

	withdrawalTx := db.Begin()
	require.NoError(t, withdrawalTx.Error)
	t.Cleanup(func() {
		_ = withdrawalTx.Rollback().Error
	})
	require.NoError(t, database.LockContactPolicyScope(
		withdrawalTx,
		fixture.Organization.ID,
		fixture.Contact.ID,
	))
	now := time.Now().UTC()
	event := &models.ConsentEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: fixture.Organization.ID,
		ContactID:      &fixture.Contact.ID,
		SubjectType:    "contact",
		SubjectKey:     fixture.Contact.ID.String(),
		Purpose:        string(models.ChannelPreferencePurposeService),
		Channel:        string(fixture.Account.Channel),
		Action:         models.ConsentActionWithdrawn,
		Source:         "test",
		Evidence:       models.JSONB{},
		CapturedAt:     now,
	}
	require.NoError(t, withdrawalTx.Create(event).Error)
	require.NoError(t, withdrawalTx.Create(&models.ConsentState{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: fixture.Organization.ID,
		ContactID:      &fixture.Contact.ID,
		SubjectType:    event.SubjectType,
		SubjectKey:     event.SubjectKey,
		Purpose:        event.Purpose,
		Channel:        event.Channel,
		Status:         models.ConsentStatusWithdrawn,
		LatestEventID:  event.ID,
		EffectiveAt:    now,
		Metadata:       models.JSONB{},
	}).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			worker := &Worker{DB: connection, Log: testutil.NopLogger()}
			return worker.recheckChannelAIOutboxDispatch(
				fixture.Organization.ID,
				job.ID,
				job.LockedBy,
			)
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	require.NoError(t, withdrawalTx.Commit().Error)
	select {
	case err := <-fenceDone:
		require.ErrorIs(t, err, errChannelOutboxAIPolicy)
		assert.Contains(t, err.Error(), "consent_withdrawn")
	case <-time.After(5 * time.Second):
		require.Fail(t, "AI dispatch fence did not resume after consent withdrawal committed")
	}

	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, job.Status)
}

func TestChannelAIOutboxDispatchSerializesCommittedHandover(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"handover-race-worker",
	)

	handoverTx := db.Begin()
	require.NoError(t, handoverTx.Error)
	t.Cleanup(func() {
		_ = handoverTx.Rollback().Error
	})
	require.NoError(t, database.LockContactPolicyScope(
		handoverTx,
		fixture.Organization.ID,
		fixture.Contact.ID,
	))
	require.NoError(t, handoverTx.Create(&models.AgentTransfer{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  fixture.Organization.ID,
		ContactID:       fixture.Contact.ID,
		WhatsAppAccount: fixture.Account.Name,
		PhoneNumber:     fixture.Contact.PhoneNumber,
		Status:          models.TransferStatusActive,
		Source:          models.TransferSourceManual,
		TransferredAt:   time.Now().UTC(),
	}).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			worker := &Worker{DB: connection, Log: testutil.NopLogger()}
			return worker.recheckChannelAIOutboxDispatch(
				fixture.Organization.ID,
				job.ID,
				job.LockedBy,
			)
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	require.NoError(t, handoverTx.Commit().Error)
	select {
	case err := <-fenceDone:
		require.ErrorIs(t, err, errChannelOutboxAIPolicy)
		assert.Contains(t, err.Error(), "human handover")
	case <-time.After(5 * time.Second):
		require.Fail(t, "AI dispatch fence did not resume after handover committed")
	}

	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, job.Status)
}

func TestChannelAIOutboxDispatchSerializesCommittedSettingsDisable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"settings-race-worker",
	)

	settingsTx := db.Begin()
	require.NoError(t, settingsTx.Error)
	t.Cleanup(func() {
		_ = settingsTx.Rollback().Error
	})
	require.NoError(t, settingsTx.Model(&models.ChatbotSettings{}).
		Where(
			"id = ? AND organization_id = ?",
			fixture.Settings.ID,
			fixture.Organization.ID,
		).
		Update("ai_enabled", false).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").
				Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			worker := &Worker{DB: connection, Log: testutil.NopLogger()}
			return worker.recheckChannelAIOutboxDispatch(
				fixture.Organization.ID,
				job.ID,
				job.LockedBy,
			)
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)

	require.NoError(t, settingsTx.Commit().Error)
	select {
	case err := <-fenceDone:
		require.ErrorIs(t, err, errChannelOutboxAIPolicy)
		assert.Contains(t, err.Error(), "qwen settings are disabled")
	case <-time.After(5 * time.Second):
		require.Fail(t, "AI dispatch fence did not resume after settings disable committed")
	}

	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, job.Status)
}

func TestChannelOutboxAbandonedDispatchIsNotRetried(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, message := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"abandoned-dispatch-worker",
	)
	stale := time.Now().UTC().Add(-defaultChannelOutboxLease - time.Second)
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ?", job.ID).
		Updates(map[string]any{
			"status":    models.OutboxJobStatusDispatching,
			"locked_at": stale,
		}).Error)

	worker := &Worker{DB: db, Log: testutil.NopLogger()}
	claimedID, claimed, err := worker.claimChannelOutboxJob(
		fixture.Organization.ID,
		"replacement-worker",
	)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Equal(t, uuid.Nil, claimedID)

	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Equal(t, "delivery_state_unknown", job.LastErrorCode)
	require.NoError(t, db.First(message, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)
}

func createChannelAIOutboxDispatchFixture(
	t *testing.T,
	db *gorm.DB,
	fixture *channelAIReplyFixture,
	frozenWindowEnd time.Time,
	workerID string,
) (*models.OutboxJob, *models.Message) {
	t.Helper()
	messageID := uuid.New()
	idempotencyKey := models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID)
	message := &models.Message{
		BaseModel:           models.BaseModel{ID: messageID},
		OrganizationID:      fixture.Organization.ID,
		WhatsAppAccount:     fixture.Account.Name,
		ContactID:           fixture.Contact.ID,
		ConversationID:      fixture.Conversation.ExternalConversationID,
		InboxConversationID: &fixture.Conversation.ID,
		Direction:           models.DirectionOutgoing,
		MessageType:         models.MessageTypeText,
		Content:             "Pending automatic reply",
		Status:              models.MessageStatusPending,
		Metadata: models.JSONB{
			"ai_generated":       true,
			"ai_settings_id":     fixture.Settings.ID.String(),
			"inbound_message_id": fixture.Inbound.ID.String(),
		},
	}
	require.NoError(t, db.Create(message).Error)

	outbound := channelapi.OutboundMessage{
		OrganizationID: fixture.Organization.ID,
		MessageID:      message.ID,
		IdempotencyKey: idempotencyKey,
		Purpose:        models.ChannelPreferencePurposeService,
		Conversation: channelapi.ConversationRef{
			ID:         fixture.Conversation.ID,
			ExternalID: fixture.Conversation.ExternalConversationID,
		},
		Recipient: channelapi.Participant{
			ID:         fixture.Contact.ID,
			ExternalID: fixture.Identity.ExternalID,
			Role:       models.ConversationParticipantRoleCustomer,
		},
		Parts: []channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Pending automatic reply",
		}},
		ServiceWindowEndsAt: &frozenWindowEnd,
		Metadata: map[string]any{
			"sender_role":        models.ConversationParticipantRoleBot,
			"ai_generated":       true,
			"ai_settings_id":     fixture.Settings.ID,
			"inbound_message_id": fixture.Inbound.ID,
		},
	}
	payload, digest, err := channelAIReplyOutboxPayload(outbound)
	require.NoError(t, err)
	now := time.Now().UTC()
	job := &models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID,
		ConversationID:   fixture.Conversation.ID,
		MessageID:        &message.ID,
		IdempotencyKey:   idempotencyKey,
		PayloadDigest:    digest,
		Purpose:          models.ChannelPreferencePurposeService,
		Status:           models.OutboxJobStatusProcessing,
		AvailableAt:      now,
		LockedAt:         &now,
		LockedBy:         workerID,
		MaxAttempts:      8,
		Payload:          payload,
	}
	require.NoError(t, db.Create(job).Error)
	return job, message
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

func configureThreadsDispatchFenceFixture(
	t *testing.T,
	db *gorm.DB,
	orgID uuid.UUID,
	account *models.ChannelAccount,
	job *models.OutboxJob,
	accessToken string,
) models.ChannelCredential {
	t.Helper()
	appID := strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		Updates(map[string]any{
			"channel":  models.ChannelThreads,
			"provider": channelapi.ThreadsProvider,
			"status":   models.ChannelAccountStatusActive,
			"config":   models.JSONB{"outbound_enabled": true},
			"metadata": models.JSONB{"app_id": appID},
		}).Error)
	account.Channel = models.ChannelThreads
	account.Provider = channelapi.ThreadsProvider
	account.Status = models.ChannelAccountStatusActive
	account.Config = models.JSONB{"outbound_enabled": true}
	account.Metadata = models.JSONB{"app_id": appID}

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": accessToken},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test:v1",
		ExpiresAt:        &expiresAt,
		Metadata:         models.JSONB{"app_id": appID},
	}
	require.NoError(t, db.Create(&credential).Error)
	require.NoError(t, db.Create(&models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Provider:       channelapi.ThreadsProvider,
		ThreadsAppID:   &appID,
		Enabled:        true,
		Config:         models.JSONB{"app_id": appID},
		CredentialData: models.JSONB{},
	}).Error)
	enableChannelAIReplyOmnichannel(t, db, orgID)
	var subscription models.Subscription
	require.NoError(t, db.Where("organization_id = ?", orgID).
		First(&subscription).Error)
	subscription.EntitlementsSnapshot[channelapi.ThreadsPublicEngagementEntitlementKey] = true
	require.NoError(t, db.Model(&subscription).
		Update("entitlements_snapshot", subscription.EntitlementsSnapshot).Error)

	providerState := models.JSONB{"container_id": "container-" + uuid.NewString()}
	require.NoError(t, db.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", job.ID, orgID).
		Update("provider_state", providerState).Error)
	job.ProviderState = providerState
	return credential
}

func configureMetaRelayDispatchFenceFixture(
	t *testing.T,
	db *gorm.DB,
	orgID uuid.UUID,
	account *models.ChannelAccount,
) *Worker {
	t.Helper()
	externalID := "7" + strings.ReplaceAll(uuid.NewString(), "-", "")[:14]
	relayBaseURL := "https://app.rereply.app/meta-relay"
	relayURL := relayBaseURL + "/v1/accounts/messenger/" + externalID
	healthAt := time.Now().UTC().Add(-time.Minute)
	inboundAt := healthAt.Add(time.Second)
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		Updates(map[string]any{
			"channel":             models.ChannelMessenger,
			"provider":            channelapi.RelayProvider,
			"external_account_id": externalID,
			"status":              models.ChannelAccountStatusActive,
			"config": models.JSONB{
				"relay_url":             relayURL,
				"identity_confirmed_id": externalID,
				"outbound_enabled":      true,
			},
			"metadata": models.JSONB{
				channelapi.MetaProviderProofMetadataKey: channelapi.MetaProviderProofVersion,
			},
			"last_health_check_at": healthAt,
			"last_inbound_at":      inboundAt,
			"last_error":           "",
			"last_error_at":        nil,
		}).Error)
	account.Channel = models.ChannelMessenger
	account.Provider = channelapi.RelayProvider
	account.ExternalAccountID = externalID
	account.Status = models.ChannelAccountStatusActive
	account.Config = models.JSONB{
		"relay_url":             relayURL,
		"identity_confirmed_id": externalID,
		"outbound_enabled":      true,
	}
	account.Metadata = models.JSONB{
		channelapi.MetaProviderProofMetadataKey: channelapi.MetaProviderProofVersion,
	}
	account.LastHealthCheckAt = &healthAt
	account.LastInboundAt = &inboundAt

	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  "test-inbound-secret",
			"outbound_secret": "test-outbound-secret",
		},
		Status:     models.ChannelCredentialStatusActive,
		KeyVersion: "test:v1",
		Metadata:   models.JSONB{},
	}
	require.NoError(t, db.Create(&credential).Error)

	return &Worker{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{
				Environment:   "production",
				EncryptionKey: "worker-test-encryption-key",
			},
			MetaRelay: config.MetaRelayConfig{
				BaseURL:             relayBaseURL,
				ProviderProofSecret: "worker-meta-provider-proof-secret-at-least-32-bytes",
				ExpectedAccountsJSON: fmt.Sprintf(`{"accounts":[{
					"organization_id":%q,
					"meta_business_id":"300000000000099",
					"channel":"messenger",
					"external_account_id":%q,
					"rereply_account_id":%q
				}]}`, orgID.String(), externalID, account.ID.String()),
			},
		},
	}
}

func cloneChannelOutboxTestJSONB(value models.JSONB) models.JSONB {
	cloned := make(models.JSONB, len(value))
	for key, entry := range value {
		cloned[key] = entry
	}
	return cloned
}

func timePointerForWorkerTest(value time.Time) *time.Time {
	return &value
}

type preparedChannelOutboxTestSender struct {
	prepareState models.JSONB
	prepareErr   error
	publish      func(
		context.Context,
		*models.ChannelAccount,
		channelapi.OutboundMessage,
		models.JSONB,
	) (channelapi.SendResult, error)
	prepareCalls int
	publishCalls int
}

func (s *preparedChannelOutboxTestSender) PrepareSend(
	_ context.Context,
	_ *models.ChannelAccount,
	_ channelapi.OutboundMessage,
) (models.JSONB, error) {
	s.prepareCalls++
	return s.prepareState, s.prepareErr
}

func (s *preparedChannelOutboxTestSender) PublishPrepared(
	ctx context.Context,
	account *models.ChannelAccount,
	message channelapi.OutboundMessage,
	providerState models.JSONB,
) (channelapi.SendResult, error) {
	s.publishCalls++
	return s.publish(ctx, account, message, providerState)
}
