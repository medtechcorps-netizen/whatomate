package handlers

import (
	"errors"
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

func TestChannelAIEnqueueSerializesBeforeSettingsCancellation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	createCentralQwenTestSettings(t, db, org.ID)
	conversation := createAIControlConversation(t, db, org.ID)
	var account models.ChannelAccount
	require.NoError(t, db.First(
		&account,
		"id = ? AND organization_id = ?",
		conversation.ChannelAccountID,
		org.ID,
	).Error)
	message := &models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           conversation.ContactID,
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Hello",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	settings := &models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		IsEnabled:      true,
		AI: models.AIConfig{
			Enabled:  true,
			Provider: models.AIProviderQwen,
			APIKey:   "test-key",
		},
	}
	require.NoError(t, db.Create(settings).Error)

	enqueueTx := db.Begin()
	require.NoError(t, enqueueTx.Error)
	t.Cleanup(func() {
		_ = enqueueTx.Rollback().Error
	})
	require.NoError(t, lockChannelAIOrganizationScopeTx(enqueueTx, org.ID))
	serviceWindowAt := time.Now().UTC()
	require.NoError(t, enqueueChannelAIReply(
		enqueueTx,
		&account,
		conversation,
		message,
		[]channelapi.MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Hello",
		}},
		serviceWindowAt,
		serviceWindowAt,
	))

	settingsPID := make(chan int, 1)
	settingsDone := make(chan error, 1)
	go func() {
		settingsDone <- db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var pid int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				settingsPID <- 0
				return err
			}
			settingsPID <- pid
			return session.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelAIOrganizationScopeTx(tx, org.ID); err != nil {
					return err
				}
				return cancelChannelAIReplyJobsForOrganizationTx(
					tx,
					org.ID,
					"chatbot_ai_settings_changed",
				)
			})
		})
	}()

	pid := <-settingsPID
	require.Positive(t, pid)
	testutil.RequirePostgresBackendWaitingForLock(t, db, pid)
	require.NoError(t, enqueueTx.Commit().Error)

	select {
	case err := <-settingsDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "settings cancellation did not resume after enqueue committed")
	}

	var persisted models.ScheduledJob
	require.NoError(t, db.Where(
		"organization_id = ? AND idempotency_key = ?",
		org.ID,
		models.ChannelAIReplyIdempotencyKey(message.ID),
	).First(&persisted).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, persisted.Status)
	assert.Equal(t, "chatbot_ai_settings_changed", persisted.LastError)
}

func TestChannelAIEnqueueSkipsDisabledSettingsBeforeLaterEnable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, org.ID)
	var account models.ChannelAccount
	require.NoError(t, db.First(
		&account,
		"id = ? AND organization_id = ?",
		conversation.ChannelAccountID,
		org.ID,
	).Error)
	message := &models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           conversation.ContactID,
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Do not queue this",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
	settings := &models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		IsEnabled:      false,
		AI: models.AIConfig{
			Enabled:  true,
			Provider: models.AIProviderQwen,
			APIKey:   "test-key",
		},
	}
	require.NoError(t, db.Create(settings).Error)

	serviceWindowAt := time.Now().UTC()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, org.ID); err != nil {
			return err
		}
		return enqueueChannelAIReply(
			tx,
			&account,
			conversation,
			message,
			[]channelapi.MessagePart{{
				Type: models.MessagePartTypeText,
				Text: "Do not queue this",
			}},
			serviceWindowAt,
			serviceWindowAt,
		)
	}))

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, org.ID); err != nil {
			return err
		}
		return tx.Model(&models.ChatbotSettings{}).
			Where("id = ? AND organization_id = ?", settings.ID, org.ID).
			Update("is_enabled", true).Error
	}))

	var count int64
	require.NoError(t, db.Model(&models.ScheduledJob{}).
		Where(
			"organization_id = ? AND idempotency_key = ?",
			org.ID,
			models.ChannelAIReplyIdempotencyKey(message.ID),
		).
		Count(&count).Error)
	assert.Zero(t, count, "later enable must not resurrect work skipped while disabled")
}

func TestInboxConversationAIStatePauseAndResume(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, org.ID)
	userID := uuid.New()

	var pausedConfig models.JSONB
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		pausedConfig, err = setInboxConversationAIStateTx(
			tx,
			org.ID,
			conversation.ID,
			true,
			&userID,
			"manual_pause",
		)
		return err
	}))
	assert.True(t, inboxConversationAIIsPaused(pausedConfig))
	assert.Equal(
		t,
		"manual_pause",
		inboxConversationAIString(pausedConfig, models.ConversationConfigAIPauseReason),
	)
	assert.Equal(
		t,
		userID.String(),
		pausedConfig[models.ConversationConfigAIPausedByUserID],
	)

	var resumedConfig models.JSONB
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		resumedConfig, err = setInboxConversationAIStateTx(
			tx,
			org.ID,
			conversation.ID,
			false,
			&userID,
			"manual_resume",
		)
		return err
	}))
	assert.False(t, inboxConversationAIIsPaused(resumedConfig))
	assert.Empty(
		t,
		inboxConversationAIString(resumedConfig, models.ConversationConfigAIPauseReason),
	)
	assert.NotEmpty(t, resumedConfig[models.ConversationConfigAIResumedAt])
}

func TestInboxConversationManualPauseRollsBackWithMessageTransaction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, org.ID)
	job := createAIControlScheduledJob(
		t,
		db,
		org.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusPending,
	)
	outbox, outboxMessage := createAIControlOutboxJob(
		t,
		db,
		conversation,
		models.OutboxJobStatusPending,
	)
	expected := errors.New("simulate outbox failure")

	err := db.Transaction(func(tx *gorm.DB) error {
		if _, err := setInboxConversationAIStateTx(
			tx,
			org.ID,
			conversation.ID,
			true,
			nil,
			"human_reply",
		); err != nil {
			return err
		}
		return expected
	})
	require.ErrorIs(t, err, expected)

	var persisted models.InboxConversation
	require.NoError(t, db.First(&persisted, "id = ?", conversation.ID).Error)
	assert.False(
		t,
		inboxConversationAIIsPaused(persisted.Config),
		"manual pause and human message/outbox must share one atomic transaction",
	)
	var persistedJob models.ScheduledJob
	require.NoError(t, db.First(&persistedJob, "id = ?", job.ID).Error)
	assert.Equal(
		t,
		models.ScheduledJobStatusPending,
		persistedJob.Status,
		"job cancellation must roll back with the failed human reply transaction",
	)
	var persistedOutbox models.OutboxJob
	require.NoError(t, db.First(&persistedOutbox, "id = ?", outbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusPending, persistedOutbox.Status)
	var persistedOutboxMessage models.Message
	require.NoError(t, db.First(
		&persistedOutboxMessage,
		"id = ?",
		outboxMessage.ID,
	).Error)
	assert.Equal(t, models.MessageStatusPending, persistedOutboxMessage.Status)
}

func TestChannelAIReplyOptInIsExplicitAndRequiresActiveOutbound(t *testing.T) {
	enable := true
	disable := false
	account := &models.ChannelAccount{
		Channel: models.ChannelInstagram,
		Status:  models.ChannelAccountStatusPending,
		Config:  models.JSONB{"outbound_enabled": false},
	}
	require.Error(t, applyChannelAIReplyOptIn(account, &enable))
	assert.NotContains(t, account.Config, "ai_reply_enabled")

	account.Status = models.ChannelAccountStatusActive
	account.Config["outbound_enabled"] = true
	require.NoError(t, applyChannelAIReplyOptIn(account, &enable))
	assert.Equal(t, true, account.Config["ai_reply_enabled"])
	require.NoError(t, applyChannelAIReplyOptIn(account, &disable))
	assert.Equal(t, false, account.Config["ai_reply_enabled"])

	account.Channel = models.ChannelThreads
	require.Error(t, applyChannelAIReplyOptIn(account, &enable))
}

func TestChannelAIRetestClearsOutboundAndExplicitOptIn(t *testing.T) {
	account := &models.ChannelAccount{
		Config: models.JSONB{
			"outbound_enabled": true,
			"ai_reply_enabled": true,
		},
	}

	disableChannelDeliveryForRetest(account)

	assert.Equal(t, false, account.Config["outbound_enabled"])
	assert.Equal(t, false, account.Config["ai_reply_enabled"])
}

func TestChannelAIAccountDisableCancelsJobsBeforeLaterEnable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, org.ID)
	pending := createAIControlScheduledJob(
		t,
		db,
		org.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusPending,
	)
	processing := createAIControlScheduledJob(
		t,
		db,
		org.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusProcessing,
	)
	pendingOutbox, pendingMessage := createAIControlOutboxJob(
		t,
		db,
		conversation,
		models.OutboxJobStatusPending,
	)
	processingOutbox, processingMessage := createAIControlOutboxJob(
		t,
		db,
		conversation,
		models.OutboxJobStatusProcessing,
	)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.First(&account, "id = ?", conversation.ChannelAccountID).Error; err != nil {
			return err
		}
		config := cloneJSONB(account.Config)
		config["ai_reply_enabled"] = false
		if err := tx.Model(&account).Update("config", config).Error; err != nil {
			return err
		}
		return cancelChannelAIReplyJobsForAccountTx(
			tx,
			org.ID,
			account.ID,
			"channel_ai_disabled",
		)
	}))

	// A later explicit enable must not resurrect work accepted before disable.
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ?", conversation.ChannelAccountID).
		Update("config", models.JSONB{
			"outbound_enabled": true,
			"ai_reply_enabled": true,
		}).Error)
	for _, jobID := range []uuid.UUID{pending.ID, processing.ID} {
		var persisted models.ScheduledJob
		require.NoError(t, db.First(&persisted, "id = ?", jobID).Error)
		assert.Equal(t, models.ScheduledJobStatusCancelled, persisted.Status)
		assert.Equal(t, "channel_ai_disabled", persisted.LastError)
		assert.Empty(t, persisted.LockedBy)
		assert.Nil(t, persisted.LockedAt)
		require.NotNil(t, persisted.CompletedAt)
	}
	for index, outboxID := range []uuid.UUID{
		pendingOutbox.ID,
		processingOutbox.ID,
	} {
		var persisted models.OutboxJob
		require.NoError(t, db.First(&persisted, "id = ?", outboxID).Error)
		assert.Equal(t, models.OutboxJobStatusCancelled, persisted.Status)
		assert.Equal(t, "ai_reply_cancelled", persisted.LastErrorCode)
		assert.Empty(t, persisted.LockedBy)
		assert.Nil(t, persisted.LockedAt)

		messageID := []uuid.UUID{pendingMessage.ID, processingMessage.ID}[index]
		var message models.Message
		require.NoError(t, db.First(&message, "id = ?", messageID).Error)
		assert.Equal(t, models.MessageStatusFailed, message.Status)
	}
}

func TestChannelAISettingsChangeCancelsOnlyTenantJobs(t *testing.T) {
	db := testutil.SetupTestDB(t)
	targetOrg := testutil.CreateTestOrganization(t, db)
	otherOrg := testutil.CreateTestOrganization(t, db)
	targetConversation := createAIControlConversation(t, db, targetOrg.ID)
	otherConversation := createAIControlConversation(t, db, otherOrg.ID)
	targetScheduled := createAIControlScheduledJob(
		t,
		db,
		targetOrg.ID,
		targetConversation.ChannelAccountID,
		targetConversation.ID,
		models.ScheduledJobStatusPending,
	)
	otherScheduled := createAIControlScheduledJob(
		t,
		db,
		otherOrg.ID,
		otherConversation.ChannelAccountID,
		otherConversation.ID,
		models.ScheduledJobStatusPending,
	)
	targetOutbox, targetMessage := createAIControlOutboxJob(
		t,
		db,
		targetConversation,
		models.OutboxJobStatusRetrying,
	)
	otherOutbox, otherMessage := createAIControlOutboxJob(
		t,
		db,
		otherConversation,
		models.OutboxJobStatusPending,
	)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return cancelChannelAIReplyJobsForOrganizationTx(
			tx,
			targetOrg.ID,
			"chatbot_ai_settings_changed",
		)
	}))

	require.NoError(t, db.First(targetScheduled, "id = ?", targetScheduled.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, targetScheduled.Status)
	require.NoError(t, db.First(targetOutbox, "id = ?", targetOutbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, targetOutbox.Status)
	require.NoError(t, db.First(targetMessage, "id = ?", targetMessage.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, targetMessage.Status)

	require.NoError(t, db.First(otherScheduled, "id = ?", otherScheduled.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusPending, otherScheduled.Status)
	require.NoError(t, db.First(otherOutbox, "id = ?", otherOutbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusPending, otherOutbox.Status)
	require.NoError(t, db.First(otherMessage, "id = ?", otherMessage.ID).Error)
	assert.Equal(t, models.MessageStatusPending, otherMessage.Status)
}

func TestChannelAIPauseCancelsJobBeforeLaterResume(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	conversation := createAIControlConversation(t, db, org.ID)
	job := createAIControlScheduledJob(
		t,
		db,
		org.ID,
		conversation.ChannelAccountID,
		conversation.ID,
		models.ScheduledJobStatusPending,
	)
	outbox, message := createAIControlOutboxJob(
		t,
		db,
		conversation,
		models.OutboxJobStatusRetrying,
	)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := setInboxConversationAIStateTx(
			tx,
			org.ID,
			conversation.ID,
			true,
			nil,
			"manual_pause",
		)
		return err
	}))
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := setInboxConversationAIStateTx(
			tx,
			org.ID,
			conversation.ID,
			false,
			nil,
			"manual_resume",
		)
		return err
	}))

	var persisted models.ScheduledJob
	require.NoError(t, db.First(&persisted, "id = ?", job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, persisted.Status)
	assert.Equal(t, "conversation_ai_paused", persisted.LastError)
	var persistedOutbox models.OutboxJob
	require.NoError(t, db.First(&persistedOutbox, "id = ?", outbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, persistedOutbox.Status)
	var persistedMessage models.Message
	require.NoError(t, db.First(&persistedMessage, "id = ?", message.ID).Error)
	assert.Equal(t, models.MessageStatusFailed, persistedMessage.Status)
}

func createAIControlConversation(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) *models.InboxConversation {
	t.Helper()
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		Channel:           models.ChannelInstagram,
		Provider:          "relay",
		Name:              "ai-control-" + uuid.NewString(),
		ExternalAccountID: "page-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{"outbound_enabled": true, "ai_reply_enabled": true},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		PhoneNumber:     "contact-" + uuid.NewString(),
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(contact).Error)
	windowEnd := time.Now().UTC().Add(time.Hour)
	conversation := &models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         organizationID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		Channel:                account.Channel,
		ExternalConversationID: "thread-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		ServiceWindowEndsAt:    &windowEnd,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(conversation).Error)
	return conversation
}

func createAIControlScheduledJob(
	t *testing.T,
	db *gorm.DB,
	organizationID, channelAccountID, conversationID uuid.UUID,
	status models.ScheduledJobStatus,
) *models.ScheduledJob {
	t.Helper()
	acceptedAt := time.Now().UTC()
	messageID := uuid.New()
	job := &models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Kind:           models.ScheduledJobKindChannelAIReply,
		AggregateType:  models.ChannelAIReplyAggregateType,
		AggregateID:    &messageID,
		RunAt:          acceptedAt,
		Status:         status,
		MaxAttempts:    5,
		IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID),
		Payload: models.JSONB{
			"organization_id":    organizationID.String(),
			"channel_account_id": channelAccountID.String(),
			"conversation_id":    conversationID.String(),
			"inbound_message_id": messageID.String(),
			"service_window_at":  acceptedAt,
		},
		Version: 1,
	}
	if status == models.ScheduledJobStatusProcessing {
		job.LockedBy = "test-worker"
		job.LockedAt = &acceptedAt
	}
	require.NoError(t, db.Create(job).Error)
	return job
}

func createAIControlOutboxJob(
	t *testing.T,
	db *gorm.DB,
	conversation *models.InboxConversation,
	status models.OutboxJobStatus,
) (*models.OutboxJob, *models.Message) {
	t.Helper()
	var account models.ChannelAccount
	require.NoError(t, db.First(
		&account,
		"id = ?",
		conversation.ChannelAccountID,
	).Error)

	message := &models.Message{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      conversation.OrganizationID,
		WhatsAppAccount:     account.Name,
		ContactID:           conversation.ContactID,
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionOutgoing,
		MessageType:         models.MessageTypeText,
		Content:             "Pending automatic reply",
		Status:              models.MessageStatusPending,
		Metadata:            models.JSONB{"ai_generated": true},
	}
	require.NoError(t, db.Create(message).Error)

	now := time.Now().UTC()
	idempotencyKey := models.ChannelAIReplyIdempotencyKey(uuid.New())
	job := &models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   conversation.OrganizationID,
		ChannelAccountID: conversation.ChannelAccountID,
		ConversationID:   conversation.ID,
		MessageID:        &message.ID,
		IdempotencyKey:   idempotencyKey,
		PayloadDigest:    "ai-control-digest",
		Purpose:          models.ChannelPreferencePurposeService,
		Status:           status,
		AvailableAt:      now,
		MaxAttempts:      8,
		Payload: models.JSONB{
			"idempotency_key": idempotencyKey,
			"metadata": map[string]any{
				"ai_generated": true,
				"sender_role":  models.ConversationParticipantRoleBot,
			},
		},
	}
	if status == models.OutboxJobStatusProcessing {
		job.LockedAt = &now
		job.LockedBy = "test-worker"
	}
	require.NoError(t, db.Create(job).Error)
	return job, message
}
