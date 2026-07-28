package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type channelAIReplyRoundTripper func(*http.Request) (*http.Response, error)

func (fn channelAIReplyRoundTripper) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return fn(request)
}

type channelAIReplyFixture struct {
	Organization *models.Organization
	Account      *models.ChannelAccount
	Contact      *models.Contact
	Identity     *models.ContactIdentity
	Conversation *models.InboxConversation
	Inbound      *models.Message
	Settings     *models.ChatbotSettings
	Job          *models.ScheduledJob
}

func TestChannelAIReplyCreatesOneBotMessageAndOutbox(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	var requestBody string
	var requestCount atomic.Int32
	worker := channelAIReplyTestWorker(db, func(request *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		encoded, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requestBody = string(encoded)
		return channelAIReplyQwenResponse("Terima kasih. Kami boleh bantu aturkan tempahan."), nil
	})

	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		"ai-success",
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, fixture.Job.ID, jobID)
	require.NoError(t, worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		"ai-success",
	))

	require.EqualValues(t, 1, requestCount.Load())
	assert.Contains(t, requestBody, `"max_tokens":160`)
	assert.Contains(t, requestBody, "plain text only")
	assert.Contains(t, requestBody, "1-3 short sentences")
	assert.Contains(t, requestBody, "Never diagnose")
	assert.Contains(t, requestBody, "Bahasa Melayu Malaysia")
	assert.Contains(t, requestBody, "never use Bahasa Indonesia")

	var persistedJob models.ScheduledJob
	require.NoError(t, db.First(&persistedJob, "id = ?", fixture.Job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, persistedJob.Status)
	assert.Equal(t, 1, persistedJob.Attempts)

	idempotencyKey := models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID)
	var outboxJobs []models.OutboxJob
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
		idempotencyKey,
	).Find(&outboxJobs).Error)
	require.Len(t, outboxJobs, 1)
	assert.Equal(t, models.OutboxJobStatusPending, outboxJobs[0].Status)
	assert.Equal(t, models.ChannelPreferencePurposeService, outboxJobs[0].Purpose)

	var botMessages []models.Message
	require.NoError(t, db.Where(
		"organization_id = ? AND id = ?",
		fixture.Organization.ID,
		channelAIReplyMessageID(idempotencyKey),
	).Find(&botMessages).Error)
	require.Len(t, botMessages, 1)
	assert.Nil(t, botMessages[0].SentByUserID)
	assert.Equal(t, models.DirectionOutgoing, botMessages[0].Direction)
	assert.Equal(t, "Terima kasih. Kami boleh bantu aturkan tempahan.", botMessages[0].Content)
}

func TestChannelAIReplyOutboxFreezesOriginatingInboundWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)

	var scheduledPayload models.ChannelAIReplyJobPayload
	require.NoError(t, decodeChannelAIReplyJSON(
		fixture.Job.Payload,
		&scheduledPayload,
	))
	originalWindowEnd := channelapi.InboundServiceWindowEndsAt(
		fixture.Account.Channel,
		scheduledPayload.ServiceWindowAt,
	)
	require.NotNil(t, originalWindowEnd)

	// Simulate a newer customer message reopening the conversation before the
	// older job finalizes. Its later deadline must not be copied into the older
	// AI reply's outbox payload.
	reopenedWindowEnd := originalWindowEnd.Add(6 * time.Hour)
	require.NoError(t, db.Model(&models.InboxConversation{}).
		Where(
			"id = ? AND organization_id = ?",
			fixture.Conversation.ID,
			fixture.Organization.ID,
		).
		Update("service_window_ends_at", reopenedWindowEnd).Error)

	const workerID = "ai-frozen-window"
	worker := channelAIReplyTestWorker(db, nil)
	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, fixture.Job.ID, jobID)
	require.NoError(t, worker.finalizeChannelAIReply(
		fixture.Organization.ID,
		jobID,
		workerID,
		"Reply for the originating inbound message",
	))

	var outbox models.OutboxJob
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
		models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID),
	).First(&outbox).Error)
	outbound, err := channelOutboundMessageForJob(&outbox)
	require.NoError(t, err)
	require.NotNil(t, outbound.ServiceWindowEndsAt)
	assert.Equal(
		t,
		originalWindowEnd.UTC(),
		outbound.ServiceWindowEndsAt.UTC(),
	)
	assert.NotEqual(t, reopenedWindowEnd.UTC(), outbound.ServiceWindowEndsAt.UTC())
}

func TestChannelAIReplyRetryUsesStableIdempotency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	var requestCount atomic.Int32
	worker := channelAIReplyTestWorker(db, func(_ *http.Request) (*http.Response, error) {
		if requestCount.Add(1) == 1 {
			return nil, errors.New("temporary DashScope failure")
		}
		return channelAIReplyQwenResponse("A short reply"), nil
	})

	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		"ai-retry-1",
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		"ai-retry-1",
	))

	var persistedJob models.ScheduledJob
	require.NoError(t, db.First(&persistedJob, "id = ?", fixture.Job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusPending, persistedJob.Status)
	assert.Equal(t, 1, persistedJob.Attempts)
	require.NoError(t, db.Model(&models.ScheduledJob{}).
		Where("id = ?", fixture.Job.ID).
		Update("run_at", time.Now().UTC().Add(-time.Second)).Error)

	jobID, claimed, err = worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		"ai-retry-2",
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		"ai-retry-2",
	))

	require.NoError(t, db.First(&persistedJob, "id = ?", fixture.Job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, persistedJob.Status)
	assert.Equal(t, 2, persistedJob.Attempts)
	assert.EqualValues(t, 2, requestCount.Load())

	idempotencyKey := models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID)
	var outboxCount, messageCount int64
	require.NoError(t, db.Model(&models.OutboxJob{}).Where(
		"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
		fixture.Organization.ID,
		fixture.Account.ID,
		idempotencyKey,
	).Count(&outboxCount).Error)
	require.NoError(t, db.Model(&models.Message{}).Where(
		"organization_id = ? AND id = ?",
		fixture.Organization.ID,
		channelAIReplyMessageID(idempotencyKey),
	).Count(&messageCount).Error)
	assert.EqualValues(t, 1, outboxCount)
	assert.EqualValues(t, 1, messageCount)
}

func TestChannelAIReplyPolicyCancellationBeforeGeneration(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		mutate     func(*testing.T, *gorm.DB, *channelAIReplyFixture)
		wantReason string
	}{
		{
			name: "missing explicit account opt in",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				config := models.JSONB{"outbound_enabled": true}
				require.NoError(t, db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.Account.ID).
					Update("config", config).Error)
			},
			wantReason: "channel_ai_disabled",
		},
		{
			name: "tenant binding mismatch",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				payload := models.JSONB{}
				for key, value := range fixture.Job.Payload {
					payload[key] = value
				}
				payload["organization_id"] = uuid.NewString()
				require.NoError(t, db.Model(&models.ScheduledJob{}).
					Where("id = ?", fixture.Job.ID).
					Update("payload", payload).Error)
			},
			wantReason: "tenant_binding_mismatch",
		},
		{
			name: "conversation paused",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				config := models.JSONB{models.ConversationConfigAIPaused: true}
				require.NoError(t, db.Model(&models.InboxConversation{}).
					Where("id = ?", fixture.Conversation.ID).
					Update("config", config).Error)
			},
			wantReason: "conversation_ai_paused",
		},
		{
			name: "active human handover",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				transfer := models.AgentTransfer{
					BaseModel:       models.BaseModel{ID: uuid.New()},
					OrganizationID:  fixture.Organization.ID,
					ContactID:       fixture.Contact.ID,
					WhatsAppAccount: fixture.Account.Name,
					PhoneNumber:     fixture.Contact.PhoneNumber,
					Status:          models.TransferStatusActive,
					Source:          models.TransferSourceManual,
					TransferredAt:   time.Now().UTC(),
				}
				require.NoError(t, db.Create(&transfer).Error)
			},
			wantReason: "human_handover_active",
		},
		{
			name: "expired service window",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				expired := time.Now().UTC().Add(-time.Second)
				require.NoError(t, db.Model(&models.InboxConversation{}).
					Where("id = ?", fixture.Conversation.ID).
					Update("service_window_ends_at", expired).Error)
			},
			wantReason: "service_window_expired",
		},
		{
			name: "later inbound cannot reopen this jobs acceptance window",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				payload := models.JSONB{}
				for key, value := range fixture.Job.Payload {
					payload[key] = value
				}
				payload["service_window_at"] = time.Now().UTC().
					Add(-channelapi.MetaCustomerServiceWindow - time.Minute)
				reopenedUntil := time.Now().UTC().Add(channelapi.MetaCustomerServiceWindow)
				require.NoError(t, db.Model(&models.ScheduledJob{}).
					Where("id = ?", fixture.Job.ID).
					Update("payload", payload).Error)
				require.NoError(t, db.Model(&models.InboxConversation{}).
					Where("id = ?", fixture.Conversation.ID).
					Update("service_window_ends_at", reopenedUntil).Error)
			},
			wantReason: "inbound_service_window_expired",
		},
		{
			name: "newer human reply",
			mutate: func(t *testing.T, db *gorm.DB, fixture *channelAIReplyFixture) {
				t.Helper()
				createChannelAIReplyHumanMessage(t, db, fixture)
			},
			wantReason: "newer_human_reply_exists",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			fixture := createChannelAIReplyWorkerFixture(t, db)
			testCase.mutate(t, db, fixture)
			var requestCount atomic.Int32
			worker := channelAIReplyTestWorker(db, func(_ *http.Request) (*http.Response, error) {
				requestCount.Add(1)
				return channelAIReplyQwenResponse("must not be called"), nil
			})
			jobID, claimed, err := worker.claimChannelAIReplyJob(
				fixture.Organization.ID,
				"ai-policy",
			)
			require.NoError(t, err)
			require.True(t, claimed)
			require.NoError(t, worker.processChannelAIReplyJob(
				context.Background(),
				fixture.Organization.ID,
				jobID,
				"ai-policy",
			))

			var persisted models.ScheduledJob
			require.NoError(t, db.First(&persisted, "id = ?", fixture.Job.ID).Error)
			assert.Equal(t, models.ScheduledJobStatusCancelled, persisted.Status)
			assert.Equal(t, testCase.wantReason, persisted.LastError)
			assert.Zero(t, requestCount.Load())
		})
	}
}

func TestChannelAIReplyFinalRecheckStopsHumanRace(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	var requestCount atomic.Int32
	worker := channelAIReplyTestWorker(db, func(_ *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		createChannelAIReplyHumanMessage(t, db, fixture)
		return channelAIReplyQwenResponse("stale AI reply"), nil
	})

	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		"ai-human-race",
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		"ai-human-race",
	))

	var persisted models.ScheduledJob
	require.NoError(t, db.First(&persisted, "id = ?", fixture.Job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, persisted.Status)
	assert.Equal(t, "newer_human_reply_exists", persisted.LastError)
	assert.EqualValues(t, 1, requestCount.Load())

	var outboxCount int64
	require.NoError(t, db.Model(&models.OutboxJob{}).Where(
		"organization_id = ? AND idempotency_key = ?",
		fixture.Organization.ID,
		models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID),
	).Count(&outboxCount).Error)
	assert.Zero(t, outboxCount)
}

func TestChannelAIReplyCommittedCancellationBlocksProcessingFinalizer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	workerID := "ai-cancel-race"
	worker := channelAIReplyTestWorker(db, func(_ *http.Request) (*http.Response, error) {
		now := time.Now().UTC()
		require.NoError(t, db.Model(&models.ScheduledJob{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND locked_by = ?",
				fixture.Job.ID,
				fixture.Organization.ID,
				models.ScheduledJobStatusProcessing,
				workerID,
			).
			Updates(map[string]any{
				"status":       models.ScheduledJobStatusCancelled,
				"completed_at": now,
				"last_error":   "conversation_ai_paused",
				"locked_at":    nil,
				"locked_by":    "",
			}).Error)
		return channelAIReplyQwenResponse("must not be queued"), nil
	})

	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		workerID,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	err = worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		workerID,
	)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	var outboxCount int64
	require.NoError(t, db.Model(&models.OutboxJob{}).Where(
		"organization_id = ? AND idempotency_key = ?",
		fixture.Organization.ID,
		models.ChannelAIReplyIdempotencyKey(fixture.Inbound.ID),
	).Count(&outboxCount).Error)
	assert.Zero(t, outboxCount)
}

func TestResolveChannelAIReplySettingsUsesSafeFallbackPrecedence(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	now := time.Now().UTC()
	olderQwen := createChannelAIReplySettings(
		t,
		db,
		org.ID,
		"legacy-one",
		models.AIProviderQwen,
		true,
		now.Add(-time.Hour),
	)
	newerQwen := createChannelAIReplySettings(
		t,
		db,
		org.ID,
		"legacy-two",
		models.AIProviderQwen,
		true,
		now,
	)
	_ = createChannelAIReplySettings(
		t,
		db,
		org.ID,
		"disabled-newest",
		models.AIProviderQwen,
		false,
		now.Add(time.Hour),
	)
	_ = createChannelAIReplySettings(
		t,
		db,
		org.ID,
		"wrong-provider",
		models.AIProviderOpenAI,
		true,
		now.Add(2*time.Hour),
	)

	resolved, err := resolveChannelAIReplySettings(db, org.ID, "social-profile")
	require.NoError(t, err)
	assert.Equal(t, newerQwen.ID, resolved.ID)
	assert.NotEqual(t, olderQwen.ID, resolved.ID)

	explicitDisabled := createChannelAIReplySettings(
		t,
		db,
		org.ID,
		"social-profile",
		models.AIProviderQwen,
		false,
		now.Add(3*time.Hour),
	)
	resolved, err = resolveChannelAIReplySettings(db, org.ID, "social-profile")
	require.NoError(t, err)
	assert.Equal(t, explicitDisabled.ID, resolved.ID)
	assert.False(t, resolved.IsEnabled, "explicit disabled profile must block fallback")
}

func TestChannelAIReplyPromptUsesResolvedProfileContextAndTenantScope(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	profileName := "legacy-qwen-" + uuid.NewString()
	require.NoError(t, db.Model(&models.Contact{}).
		Where("id = ?", fixture.Contact.ID).
		Update("profile_name", "").Error)
	require.NoError(t, db.Model(&models.ChatbotSettings{}).
		Where("id = ?", fixture.Settings.ID).
		Update("whats_app_account", profileName).Error)
	fixture.Settings.WhatsAppAccount = profileName
	aiContext := models.AIContext{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  fixture.Organization.ID,
		WhatsAppAccount: profileName,
		Name:            "ReAlign Services and Pricing",
		IsEnabled:       true,
		Priority:        100,
		ContextType:     models.ContextTypeStatic,
		StaticContent:   "TENANT-SAFE-KB-CONTENT",
		ApiConfig:       models.JSONB{},
	}
	require.NoError(t, db.Create(&aiContext).Error)
	otherOrg := testutil.CreateTestOrganization(t, db)
	otherContext := aiContext
	otherContext.BaseModel = models.BaseModel{ID: uuid.New()}
	otherContext.OrganizationID = otherOrg.ID
	otherContext.StaticContent = "OTHER-TENANT-SECRET"
	require.NoError(t, db.Create(&otherContext).Error)

	var requestBody string
	worker := channelAIReplyTestWorker(db, func(request *http.Request) (*http.Response, error) {
		encoded, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requestBody = string(encoded)
		return channelAIReplyQwenResponse("Reply"), nil
	})
	jobID, claimed, err := worker.claimChannelAIReplyJob(
		fixture.Organization.ID,
		"ai-context",
	)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, worker.processChannelAIReplyJob(
		context.Background(),
		fixture.Organization.ID,
		jobID,
		"ai-context",
	))

	assert.Contains(t, requestBody, "ReAlign Services and Pricing")
	assert.Contains(t, requestBody, "TENANT-SAFE-KB-CONTENT")
	assert.Contains(t, requestBody, channelAISocialSafetySuffix)
	assert.NotContains(t, requestBody, "OTHER-TENANT-SECRET")
	assert.NotContains(t, requestBody, fixture.Contact.PhoneNumber)
	assert.NotContains(t, requestBody, fixture.Identity.ExternalID)
	assert.NotContains(t, requestBody, fixture.Identity.Address)
}

func createChannelAIReplyWorkerFixture(
	t *testing.T,
	db *gorm.DB,
) *channelAIReplyFixture {
	t.Helper()
	organization := testutil.CreateTestOrganization(t, db)
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelInstagram,
		Provider:          channelapi.RelayProvider,
		Name:              "social-" + uuid.NewString(),
		ExternalAccountID: "page-" + uuid.NewString(),
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{"text": true, "service_window": true},
		Config: models.JSONB{
			"outbound_enabled": true,
			"ai_reply_enabled": true,
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(account).Error)
	credential := &models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindPrimary,
		Version:          1,
		CredentialBlob:   models.JSONB{"outbound_secret": "test"},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test",
		Metadata:         models.JSONB{},
	}
	require.NoError(t, db.Create(credential).Error)
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		PhoneNumber:     "external-" + uuid.NewString(),
		ProfileName:     "Amina",
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(contact).Error)
	identity := &models.ContactIdentity{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ContactID:        contact.ID,
		ChannelAccountID: account.ID,
		Channel:          account.Channel,
		ExternalID:       "ig-user-" + uuid.NewString(),
		Address:          "ig-user",
		DisplayName:      contact.ProfileName,
		Metadata:         models.JSONB{},
	}
	require.NoError(t, db.Create(identity).Error)
	windowEnd := time.Now().UTC().Add(time.Hour)
	conversation := &models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         organization.ID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		ContactIdentityID:      &identity.ID,
		Channel:                account.Channel,
		ExternalConversationID: "conversation-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC().Add(-time.Hour),
		ServiceWindowEndsAt:    &windowEnd,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(conversation).Error)
	inboundAt := time.Now().UTC().Add(-time.Minute)
	inbound := &models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: inboundAt,
			UpdatedAt: inboundAt,
		},
		OrganizationID:      organization.ID,
		WhatsAppAccount:     account.Name,
		ContactID:           contact.ID,
		WhatsAppMessageID:   "provider-" + uuid.NewString(),
		ConversationID:      conversation.ExternalConversationID,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Boleh saya buat appointment?",
		Status:              models.MessageStatusReceived,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(inbound).Error)
	settings := createChannelAIReplySettings(
		t,
		db,
		organization.ID,
		account.Name,
		models.AIProviderQwen,
		true,
		time.Now().UTC(),
	)
	settings.AI.MaxTokens = 900
	settings.AI.IncludeHistory = true
	settings.AI.HistoryLimit = 4
	settings.AI.SystemPrompt = "Reply in English or Bahasa Melayu Malaysia."
	require.NoError(t, db.Save(settings).Error)
	enableChannelAIReplyOmnichannel(t, db, organization.ID)

	payload := models.JSONB{
		"organization_id":    organization.ID.String(),
		"channel_account_id": account.ID.String(),
		"conversation_id":    conversation.ID.String(),
		"inbound_message_id": inbound.ID.String(),
		"service_window_at":  inboundAt,
	}
	inboundID := inbound.ID
	job := &models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Kind:           models.ScheduledJobKindChannelAIReply,
		AggregateType:  models.ChannelAIReplyAggregateType,
		AggregateID:    &inboundID,
		RunAt:          time.Now().UTC().Add(-time.Second),
		Status:         models.ScheduledJobStatusPending,
		MaxAttempts:    5,
		IdempotencyKey: models.ChannelAIReplyIdempotencyKey(inbound.ID),
		Payload:        payload,
		Version:        1,
	}
	require.NoError(t, db.Create(job).Error)
	return &channelAIReplyFixture{
		Organization: organization,
		Account:      account,
		Contact:      contact,
		Identity:     identity,
		Conversation: conversation,
		Inbound:      inbound,
		Settings:     settings,
		Job:          job,
	}
}

func createChannelAIReplySettings(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	accountName string,
	provider models.AIProvider,
	enabled bool,
	updatedAt time.Time,
) *models.ChatbotSettings {
	t.Helper()
	settings := &models.ChatbotSettings{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: updatedAt,
			UpdatedAt: updatedAt,
		},
		OrganizationID:  organizationID,
		WhatsAppAccount: accountName,
		IsEnabled:       enabled,
		AI: models.AIConfig{
			Enabled:     enabled,
			Provider:    provider,
			APIKey:      "test-qwen-key",
			Model:       "qwen-test",
			MaxTokens:   300,
			Temperature: 0.2,
		},
	}
	require.NoError(t, db.Create(settings).Error)
	return settings
}

func enableChannelAIReplyOmnichannel(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) {
	t.Helper()
	plan := &models.Plan{
		BaseModel: models.BaseModel{ID: uuid.New()},
		ScopeKey:  "test-" + uuid.NewString(),
		Code:      "test-" + uuid.NewString(),
		Name:      "AI reply test plan",
		Status:    models.CommercialPlanStatusActive,
		Vertical:  "healthcare",
		Metadata:  models.JSONB{},
	}
	require.NoError(t, db.Create(plan).Error)
	billing := &models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(billing).Error)
	start := time.Now().UTC().Add(-time.Hour)
	end := time.Now().UTC().Add(24 * time.Hour)
	subscription := &models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organizationID,
		BillingAccountID:     billing.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "send_invoice",
		EntitlementsSnapshot: models.JSONB{channelapi.OmnichannelEntitlementKey: true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &start,
		CurrentPeriodEnd:     &end,
	}
	require.NoError(t, db.Create(subscription).Error)
}

func createChannelAIReplyHumanMessage(
	t *testing.T,
	db *gorm.DB,
	fixture *channelAIReplyFixture,
) {
	t.Helper()
	user := &models.User{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: fixture.Organization.ID,
		Email:          "human-" + uuid.NewString() + "@example.test",
		FullName:       "Human Agent",
		IsActive:       true,
		Settings:       models.JSONB{},
	}
	require.NoError(t, db.Create(user).Error)
	message := &models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
		OrganizationID:      fixture.Organization.ID,
		WhatsAppAccount:     fixture.Account.Name,
		ContactID:           fixture.Contact.ID,
		ConversationID:      fixture.Conversation.ExternalConversationID,
		InboxConversationID: &fixture.Conversation.ID,
		Direction:           models.DirectionOutgoing,
		MessageType:         models.MessageTypeText,
		Content:             "A human already replied",
		Status:              models.MessageStatusPending,
		SentByUserID:        &user.ID,
		Metadata:            models.JSONB{},
	}
	require.NoError(t, db.Create(message).Error)
}

func channelAIReplyTestWorker(
	db *gorm.DB,
	roundTrip channelAIReplyRoundTripper,
) *Worker {
	cfg := &config.Config{}
	cfg.AI.QwenBaseURL = "https://qwen.example.test/compatible-mode/v1"
	return &Worker{
		Config:   cfg,
		DB:       db,
		Log:      testutil.NopLogger(),
		QwenHTTP: &http.Client{Transport: roundTrip},
	}
}

func channelAIReplyQwenResponse(text string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"content":` + quoteChannelAIReplyTestJSON(text) + `}}]}`,
		)),
	}
}

func quoteChannelAIReplyTestJSON(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return `"` + value + `"`
}
