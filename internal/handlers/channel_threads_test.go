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
	"gorm.io/gorm/clause"
)

type threadsWebhookPersistenceFixture struct {
	DB           *gorm.DB
	Organization *models.Organization
	User         *models.User
	Integration  models.ProviderIntegration
	Account      models.ChannelAccount
	Credential   models.ChannelCredential
	AppID        string
}

func TestThreadsWebhookBindingRequiresDedicatedAppAndSingleAccount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	appID := "1771429782494480"
	require.NoError(t, db.Create(&models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &appID,
		Enabled:        true,
		Config:         models.JSONB{"app_id": appID},
		CredentialData: models.JSONB{},
	}).Error)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              "Threads binding test",
		ExternalAccountID: appID + "1",
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{"app_id": appID},
	}
	require.NoError(t, db.Create(&account).Error)
	require.NoError(t, validateThreadsWebhookAccountBinding(db, &account, appID))
	require.Error(t, validateThreadsWebhookAccountBinding(db, &account, "other-app"))

	second := account
	second.ID = uuid.New()
	second.Name = "Second Threads binding test"
	second.ExternalAccountID = "9988776655443322"
	require.NoError(t, db.Create(&second).Error)
	require.Error(t, validateThreadsWebhookAccountBinding(db, &account, appID))
}

func TestThreadsWebhookPersistenceRequiresCurrentOAuthCredential(t *testing.T) {
	fixture := newThreadsWebhookPersistenceFixture(t, "1771429782494481")

	require.NoError(t, persistThreadsWebhookTestRawEvent(
		fixture,
		"threads-current-credential",
	))

	require.NoError(t, fixture.DB.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.Credential.ID).
		Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": time.Now().UTC(),
		}).Error)
	err := persistThreadsWebhookTestRawEvent(
		fixture,
		"threads-revoked-credential",
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errThreadsWebhookBindingInactive), err)

	var count int64
	require.NoError(t, fixture.DB.Model(&models.InboundEvent{}).
		Where(
			"organization_id = ? AND channel_account_id = ?",
			fixture.Organization.ID,
			fixture.Account.ID,
		).
		Count(&count).Error)
	assert.EqualValues(t, 1, count, "the revoked-credential attempt must not persist a raw event")
}

func TestThreadsWebhookPersistenceWaitsForDisconnectAndFailsClosed(t *testing.T) {
	fixture := newThreadsWebhookPersistenceFixture(t, "1771429782494482")

	disconnectTx := fixture.DB.Begin()
	require.NoError(t, disconnectTx.Error)
	t.Cleanup(func() { _ = disconnectTx.Rollback().Error })
	var integration models.ProviderIntegration
	require.NoError(t, disconnectTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", fixture.Integration.ID, fixture.Organization.ID).
		First(&integration).Error)
	require.NoError(t, disconnectTx.Model(&models.ProviderIntegration{}).
		Where("id = ? AND organization_id = ?", integration.ID, fixture.Organization.ID).
		Update("enabled", false).Error)
	require.NoError(t, disconnectThreadsChannelAccounts(
		disconnectTx,
		fixture.Organization.ID,
		fixture.User.ID,
		time.Now().UTC(),
	))

	webhookPID := make(chan int, 1)
	webhookDone := make(chan error, 1)
	go func() {
		webhookDone <- fixture.DB.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				webhookPID <- 0
				return err
			}
			webhookPID <- backendPID
			return persistThreadsWebhookTestRawEventWithDB(
				session,
				fixture,
				"threads-concurrent-disconnect",
			)
		})
	}()

	backendPID := <-webhookPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.DB, backendPID)
	require.NoError(t, disconnectTx.Commit().Error)

	select {
	case err := <-webhookDone:
		require.Error(t, err)
		assert.True(t, errors.Is(err, errThreadsWebhookBindingInactive), err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "Threads webhook persistence did not resume after disconnect committed")
	}

	var count int64
	require.NoError(t, fixture.DB.Model(&models.InboundEvent{}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND dedupe_key = ?",
			fixture.Organization.ID,
			fixture.Account.ID,
			"threads-concurrent-disconnect",
		).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestThreadsChannelCreationRequiresAdditionalEntitlement(t *testing.T) {
	t.Parallel()

	assert.True(t, validChannelIdentifier(models.ChannelThreads))

	key, required := additionalChannelCreationEntitlement(models.ChannelThreads)
	assert.True(t, required)
	assert.Equal(t, "threads.public_engagement.enabled", key)
	assert.Equal(t, channelapi.ThreadsPublicEngagementEntitlementKey, key)

	key, required = additionalChannelCreationEntitlement(models.ChannelInstagram)
	assert.False(t, required)
	assert.Empty(t, key)
}

func TestUnsupportedChannelCreationFailsClosedUntilAdaptersExist(t *testing.T) {
	t.Parallel()

	threads := ChannelAccountRequest{
		Channel:  models.ChannelThreads,
		Provider: channelapi.RelayProvider,
	}
	err := validateChannelCreationPolicy(threads)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OAuth-managed")

	tiktok := ChannelAccountRequest{
		Channel:  models.ChannelTikTok,
		Provider: channelapi.RelayProvider,
	}
	err = validateChannelCreationPolicy(tiktok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preparation-only")

	require.NoError(t, validateChannelCreationPolicy(ChannelAccountRequest{
		Channel:  models.ChannelWebChat,
		Provider: channelapi.RelayProvider,
	}))
}

func TestThreadsChannelMetadataStaysPublicEngagementOnly(t *testing.T) {
	t.Parallel()

	config := threadsPublicEngagementConfig(models.JSONB{
		"relay_url":                 "https://relay.example.test/threads",
		"engagement_mode":           "direct_message",
		"direct_messages_supported": true,
	})
	assert.Equal(t, "https://relay.example.test/threads", config["relay_url"])
	assert.Equal(t, threadsPublicEngagementMode, config["engagement_mode"])
	assert.Equal(t, false, config["direct_messages_supported"])
	assert.Equal(t, false, config["beta"])
	assert.Equal(t, true, config["activation_available"])

	capabilities := threadsPublicEngagementCapabilities(models.JSONB{
		"text":                true,
		"business_initiation": true,
		"direct_messages":     true,
	})
	assert.Equal(t, true, capabilities["text"])
	assert.Equal(t, false, capabilities["business_initiation"])
	assert.Equal(t, false, capabilities["direct_messages"])
	assert.Equal(t, true, capabilities["public_replies"])
	assert.Equal(t, true, capabilities["mentions"])
	assert.Equal(t, true, capabilities["reply_target_required"])
}

func TestThreadsRelayAccountCannotUseFirstPartyAdapter(t *testing.T) {
	t.Parallel()

	app := &App{}
	_, err := app.channelAdapter(&models.ChannelAccount{
		Channel:  models.ChannelThreads,
		Provider: channelapi.RelayProvider,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChannelAdapterUnavailable)
	assert.Contains(t, err.Error(), "not OAuth-managed")
}

func TestThreadsPublicReplyRequiresExistingReplyOrMentionTarget(t *testing.T) {
	t.Parallel()

	conversation := &models.InboxConversation{
		Channel:                models.ChannelThreads,
		ExternalConversationID: "threads-public-target-123",
		Metadata:               models.JSONB{"engagement_type": "reply"},
		ChannelAccount: &models.ChannelAccount{
			Channel:  models.ChannelThreads,
			Provider: channelapi.ThreadsProvider,
			Config: models.JSONB{
				"engagement_mode": threadsPublicEngagementMode,
			},
		},
	}
	valid := &SendInboxConversationMessageRequest{
		ReplyToExternalID: "threads-public-target-123",
	}
	require.NoError(t, validateThreadsPublicReplyTarget(conversation, valid))
	assert.Equal(t, "threads-public-target-123", valid.ReplyToExternalID)

	mention := *conversation
	mention.Metadata = models.JSONB{"engagement_type": "mention"}
	require.NoError(t, validateThreadsPublicReplyTarget(&mention, &SendInboxConversationMessageRequest{
		ReplyToExternalID: mention.ExternalConversationID,
	}))

	err := validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "existing reply or mention target")

	err = validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{
		ReplyToExternalID: "other-public-target",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selected public conversation")

	err = validateThreadsPublicReplyTarget(conversation, &SendInboxConversationMessageRequest{
		ReplyToExternalID: " threads-public-target-123 ",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selected public conversation")

	directMessage := *conversation
	directMessage.Metadata = models.JSONB{"engagement_type": "direct_message"}
	err = validateThreadsPublicReplyTarget(&directMessage, &SendInboxConversationMessageRequest{
		ReplyToExternalID: directMessage.ExternalConversationID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "direct messages and standalone posts are not supported")

	incompatibleRelay := *conversation
	incompatibleRelay.ChannelAccount = &models.ChannelAccount{
		Channel:  models.ChannelThreads,
		Provider: "meta",
		Config:   conversation.ChannelAccount.Config,
	}
	err = validateThreadsPublicReplyTarget(&incompatibleRelay, &SendInboxConversationMessageRequest{
		ReplyToExternalID: incompatibleRelay.ExternalConversationID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OAuth-managed Threads account")
}

func newThreadsWebhookPersistenceFixture(
	t *testing.T,
	appID string,
) *threadsWebhookPersistenceFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(
		t,
		db,
		organization.ID,
		testutil.WithSuperAdmin(),
	)
	binding := appID
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &binding,
		Enabled:        true,
		Config:         models.JSONB{"app_id": appID},
		CredentialData: models.JSONB{},
		CreatedByID:    &user.ID,
		UpdatedByID:    &user.ID,
	}
	require.NoError(t, db.Create(&integration).Error)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              "Threads webhook persistence " + appID,
		ExternalAccountID: appID + "1",
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{"text": true, "replies": true},
		Config: models.JSONB{
			"engagement_mode":  threadsPublicEngagementMode,
			"outbound_enabled": true,
		},
		Metadata:    models.JSONB{"app_id": appID, "username": "clinic_account"},
		CreatedByID: &user.ID,
		UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&account).Error)
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": "encrypted-test-token"},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test:v1",
		ExpiresAt:        &expiresAt,
		Metadata:         models.JSONB{"app_id": appID},
	}
	require.NoError(t, db.Create(&credential).Error)
	return &threadsWebhookPersistenceFixture{
		DB:           db,
		Organization: organization,
		User:         user,
		Integration:  integration,
		Account:      account,
		Credential:   credential,
		AppID:        appID,
	}
}

func persistThreadsWebhookTestRawEvent(
	fixture *threadsWebhookPersistenceFixture,
	dedupeKey string,
) error {
	return persistThreadsWebhookTestRawEventWithDB(
		fixture.DB,
		fixture,
		dedupeKey,
	)
}

func persistThreadsWebhookTestRawEventWithDB(
	db *gorm.DB,
	fixture *threadsWebhookPersistenceFixture,
	dedupeKey string,
) error {
	return db.Transaction(func(tx *gorm.DB) error {
		account, err := lockThreadsWebhookPersistenceBinding(
			tx,
			fixture.Organization.ID,
			fixture.Account.ID,
			fixture.AppID,
			fixture.Account.ExternalAccountID,
			time.Now().UTC(),
		)
		if err != nil {
			return err
		}
		rawEvent := models.InboundEvent{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.Organization.ID,
			ChannelAccountID: account.ID,
			DedupeKey:        dedupeKey,
			EventType:        "raw_webhook",
			Status:           models.InboundEventStatusPending,
			SignatureValid:   true,
			ReceivedAt:       time.Now().UTC(),
			Headers:          models.JSONB{},
			Payload:          models.JSONB{"test": true},
		}
		_, _, err = persistOrClaimRawInboundEvent(tx, &rawEvent)
		return err
	})
}
