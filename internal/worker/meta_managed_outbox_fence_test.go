package worker

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	managedMetaOutboxTestAppID     = "100000000000201"
	managedMetaOutboxTestProfileID = "700000000000201"
)

var managedMetaOutboxProfileSequence atomic.Uint64

func nextManagedMetaOutboxProfileID() string {
	return strconv.FormatUint(790000000000000+managedMetaOutboxProfileSequence.Add(1), 10)
}

type managedMetaOutboxFenceFixture struct {
	db      *gorm.DB
	worker  *Worker
	org     *models.Organization
	account *models.ChannelAccount
	job     *models.OutboxJob
	oauth   models.ChannelCredential
	webhook models.ChannelCredential
}

type managedMetaAIOutboxFenceFixture struct {
	db       *gorm.DB
	fixture  *channelAIReplyFixture
	job      *models.OutboxJob
	oauth    models.ChannelCredential
	webhook  models.ChannelCredential
	expected models.ChannelAccount
	worker   *Worker
}

type managedInstagramRotatingSendAdapter struct {
	channelapi.Adapter
	send func(
		context.Context,
		*models.ChannelAccount,
		channelapi.OutboundMessage,
	) (channelapi.SendResult, error)
}

func (adapter *managedInstagramRotatingSendAdapter) Send(
	ctx context.Context,
	account *models.ChannelAccount,
	message channelapi.OutboundMessage,
) (channelapi.SendResult, error) {
	return adapter.send(ctx, account, message)
}

func TestManagedInstagramCredentialGenerationRequiresExactTiming(t *testing.T) {
	now := time.Now().UTC()
	organizationID := uuid.New()
	accountID := uuid.New()
	generation := managedMetaCredentialGeneration{
		OAuthID: uuid.New(), OAuthVersion: 1, OAuthCreatedAt: now.Add(-time.Minute),
		OAuthExpiresAt: now.Add(2 * time.Hour), OAuthHasExpiry: true,
		OAuthOrganizationID: organizationID, OAuthAccountID: accountID,
		WebhookID: uuid.New(), WebhookVersion: 1, WebhookCreatedAt: now.Add(-time.Minute),
		WebhookOrganizationID: organizationID, WebhookAccountID: accountID,
	}
	require.True(t, managedInstagramCredentialGenerationValid(
		generation,
		organizationID,
		accountID,
		now,
	))

	for _, test := range []struct {
		name   string
		mutate func(*managedMetaCredentialGeneration)
	}{
		{"missing OAuth expiry", func(value *managedMetaCredentialGeneration) { value.OAuthHasExpiry = false }},
		{"near OAuth expiry", func(value *managedMetaCredentialGeneration) { value.OAuthExpiresAt = now.Add(time.Minute) }},
		{"future OAuth creation", func(value *managedMetaCredentialGeneration) { value.OAuthCreatedAt = now.Add(2 * time.Minute) }},
		{"future webhook creation", func(value *managedMetaCredentialGeneration) { value.WebhookCreatedAt = now.Add(2 * time.Minute) }},
		{"cross-account webhook", func(value *managedMetaCredentialGeneration) { value.WebhookAccountID = uuid.New() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := generation
			test.mutate(&invalid)
			assert.False(t, managedInstagramCredentialGenerationValid(
				invalid,
				organizationID,
				accountID,
				now,
			))
		})
	}
}

func TestManagedInstagramChannelOutboxURLsRequireExactDerivedRows(t *testing.T) {
	settings := config.MetaInstagramConfig{
		ReReplyBaseURL: "https://app.example.test",
		RelayBaseURL:   "https://relay.example.test",
	}
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		ExternalAccountID: managedMetaOutboxTestProfileID,
		Config:            models.JSONB{},
	}
	account.Config["rereply_webhook_url"] = "https://app.example.test/api/webhooks/channels/" + account.ID.String()
	account.Config["relay_url"] = "https://relay.example.test/v1/accounts/instagram/" + account.ExternalAccountID
	require.True(t, managedInstagramChannelOutboxURLsAllowed(settings, &account))

	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{"userinfo", "rereply_webhook_url", "https://attacker@app.example.test/api/webhooks/channels/" + account.ID.String()},
		{"query", "rereply_webhook_url", "https://app.example.test/api/webhooks/channels/" + account.ID.String() + "?next=x"},
		{"fragment", "rereply_webhook_url", "https://app.example.test/api/webhooks/channels/" + account.ID.String() + "#suffix"},
		{"raw path", "rereply_webhook_url", "https://app.example.test/api/webhooks/%63hannels/" + account.ID.String()},
		{"suffix", "relay_url", "https://relay.example.test/v1/accounts/instagram/" + account.ExternalAccountID + "/extra"},
		{"deceptive host", "relay_url", "https://relay.example.test.attacker.test/v1/accounts/instagram/" + account.ExternalAccountID},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := account
			invalid.Config = cloneChannelOutboxTestJSONB(account.Config)
			invalid.Config[test.key] = test.value
			assert.False(t, managedInstagramChannelOutboxURLsAllowed(settings, &invalid))
		})
	}
}

func TestManagedMetaIntentDetectionIncludesEitherReservedMarker(t *testing.T) {
	account := models.ChannelAccount{
		Channel:  models.ChannelInstagram,
		Provider: channelapi.RelayProvider,
		Config:   models.JSONB{},
	}
	assert.False(t, isManagedMetaChannelOutboxAccount(&account))
	assert.False(t, exactManagedMetaChannelOutboxBinding(&account))

	account.Config["meta_registry_managed"] = true
	assert.True(t, isManagedMetaChannelOutboxAccount(&account))
	assert.False(t, exactManagedMetaChannelOutboxBinding(&account))

	account.Config = models.JSONB{
		"meta_management_mode": metaregistry.ManagementModePlatformOAuth,
	}
	assert.True(t, isManagedMetaChannelOutboxAccount(&account))
	assert.False(t, exactManagedMetaChannelOutboxBinding(&account))

	account.Config["meta_registry_managed"] = true
	assert.True(t, isManagedMetaChannelOutboxAccount(&account))
	assert.True(t, exactManagedMetaChannelOutboxBinding(&account))

	account.BaseModel = models.BaseModel{ID: uuid.New()}
	account.ExternalAccountID = managedMetaOutboxTestProfileID
	account.Config = models.JSONB{
		"instagram_api_mode":  "instagram_login",
		"rereply_webhook_url": "https://app.example.test/api/webhooks/channels/" + account.ID.String(),
		"relay_url":           "https://relay.example.test/v1/accounts/instagram/" + managedMetaOutboxTestProfileID,
	}
	account.Metadata = models.JSONB{
		"meta_platform_app_id":           managedMetaOutboxTestAppID,
		"meta_webhook_app":               "instagram_login",
		"meta_subscription_operation_id": uuid.NewString(),
	}
	assert.True(t, isManagedMetaChannelOutboxAccount(&account))
	assert.False(t, exactManagedMetaChannelOutboxBinding(&account))

	account.Config = models.JSONB{"relay_url": "https://static.example.test/instagram"}
	account.Metadata = models.JSONB{}
	assert.False(t, isManagedMetaChannelOutboxAccount(&account))

	account.Provider = "static-provider"
	assert.False(t, isManagedMetaChannelOutboxAccount(&account))
}

func TestManagedMetaManualDispatchFenceRequiresExactRuntimeGeneration(t *testing.T) {
	t.Run("exact generation crosses dispatch fence", func(t *testing.T) {
		fixture := createManagedMetaOutboxFenceFixture(t)
		fenced, err := fixture.worker.markManagedMetaChannelOutboxDispatching(
			fixture.org.ID,
			fixture.job.ID,
			fixture.job.LockedBy,
			fixture.account,
		)
		require.NoError(t, err)
		require.Len(t, fenced.Credentials, 2)
		generation, ok := managedMetaCurrentCredentialGeneration(
			fenced.Credentials,
			time.Now().UTC(),
		)
		require.True(t, ok)
		assert.Equal(t, fixture.oauth.ID, generation.OAuthID)
		assert.Equal(t, fixture.webhook.ID, generation.WebhookID)
		require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
		assert.Equal(t, models.OutboxJobStatusDispatching, fixture.job.Status)
	})

	t.Run("rotation after load fails closed even when new binding is valid", func(t *testing.T) {
		fixture := createManagedMetaOutboxFenceFixture(t)
		now := time.Now().UTC()
		require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
			Where("organization_id = ? AND channel_account_id = ?", fixture.org.ID, fixture.account.ID).
			Updates(map[string]any{
				"status":     models.ChannelCredentialStatusRevoked,
				"revoked_at": now,
			}).Error)
		expiresAt := now.Add(2 * time.Hour)
		oauth := fixture.oauth
		oauth.BaseModel = models.BaseModel{ID: uuid.New()}
		oauth.Version = 2
		oauth.Status = models.ChannelCredentialStatusActive
		oauth.RevokedAt = nil
		oauth.ExpiresAt = &expiresAt
		webhook := fixture.webhook
		webhook.BaseModel = models.BaseModel{ID: uuid.New()}
		webhook.Version = 2
		webhook.Status = models.ChannelCredentialStatusActive
		webhook.RevokedAt = nil
		require.NoError(t, fixture.db.Create(&oauth).Error)
		require.NoError(t, fixture.db.Create(&webhook).Error)
		metadata := cloneChannelOutboxTestJSONB(fixture.account.Metadata)
		metadata["meta_subscription_oauth_credential_id"] = oauth.ID.String()
		metadata["meta_subscription_oauth_version"] = oauth.Version
		metadata["meta_subscription_webhook_credential_id"] = webhook.ID.String()
		metadata["meta_subscription_webhook_version"] = webhook.Version
		require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
			Update("metadata", metadata).Error)

		_, err := fixture.worker.markManagedMetaChannelOutboxDispatching(
			fixture.org.ID,
			fixture.job.ID,
			fixture.job.LockedBy,
			fixture.account,
		)
		assert.ErrorIs(t, err, errChannelOutboxManagedMetaFence)
		require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
		assert.Equal(t, models.OutboxJobStatusProcessing, fixture.job.Status)
	})

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *managedMetaOutboxFenceFixture)
	}{
		{
			name: "quarantine runtime",
			mutate: func(_ *testing.T, fixture *managedMetaOutboxFenceFixture) {
				fixture.worker.Config.MetaInstagram.QuarantineOnly = true
			},
		},
		{
			name: "pending deauthorization",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				metadata := cloneChannelOutboxTestJSONB(fixture.account.Metadata)
				metadata["meta_deauthorization_pending_digest"] = "pending"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "pending data deletion",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				metadata := cloneChannelOutboxTestJSONB(fixture.account.Metadata)
				metadata["meta_data_deletion_pending_digest"] = "pending"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "zero OAuth creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("created_at", time.Time{}).Error)
			},
		},
		{
			name: "future OAuth creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "missing OAuth expiry",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("expires_at", nil).Error)
			},
		},
		{
			name: "near OAuth expiry",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("expires_at", time.Now().UTC().Add(time.Minute)).Error)
			},
		},
		{
			name: "zero webhook creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.webhook.ID, fixture.org.ID).
					Update("created_at", time.Time{}).Error)
			},
		},
		{
			name: "future webhook creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.webhook.ID, fixture.org.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "webhook URL userinfo",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://attacker@app.example.test/api/webhooks/channels/"+fixture.account.ID.String(),
				)
			},
		},
		{
			name: "webhook URL query",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"?next=https://attacker.example.test",
				)
			},
		},
		{
			name: "webhook URL fragment",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"#suffix",
				)
			},
		},
		{
			name: "webhook URL raw path",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/%63hannels/"+fixture.account.ID.String(),
				)
			},
		},
		{
			name: "webhook URL suffix",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"/extra",
				)
			},
		},
		{
			name: "relay URL deceptive host",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test.attacker.test/v1/accounts/instagram/"+fixture.account.ExternalAccountID,
				)
			},
		},
		{
			name: "relay URL query",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test/v1/accounts/instagram/"+fixture.account.ExternalAccountID+"?token=x",
				)
			},
		},
		{
			name: "relay URL suffix",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				updateManagedMetaOutboxURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test/v1/accounts/instagram/"+fixture.account.ExternalAccountID+"/extra",
				)
			},
		},
		{
			name: "authorizing identity mismatch",
			mutate: func(t *testing.T, fixture *managedMetaOutboxFenceFixture) {
				metadata := cloneChannelOutboxTestJSONB(fixture.account.Metadata)
				metadata["meta_authorizing_user_id"] = "700000000000299"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
	} {
		t.Run(test.name+" blocks dispatch", func(t *testing.T) {
			fixture := createManagedMetaOutboxFenceFixture(t)
			test.mutate(t, &fixture)
			_, err := fixture.worker.markManagedMetaChannelOutboxDispatching(
				fixture.org.ID,
				fixture.job.ID,
				fixture.job.LockedBy,
				fixture.account,
			)
			assert.ErrorIs(t, err, errChannelOutboxManagedMetaFence)
			require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusProcessing, fixture.job.Status)
		})
	}
}

func TestManagedInstagramDispatchingErrorAfterRotationIsTerminal(t *testing.T) {
	t.Run("manual message cannot relay under N plus 1", func(t *testing.T) {
		fixture := createManagedMetaOutboxFenceFixture(t)
		enableChannelAIReplyOmnichannel(t, fixture.db, fixture.org.ID)
		serviceWindowEnd := time.Now().UTC().Add(time.Hour)
		require.NoError(t, fixture.db.Model(&models.InboxConversation{}).Where(
			"id = ? AND organization_id = ?",
			fixture.job.ConversationID,
			fixture.org.ID,
		).Update("service_window_ends_at", serviceWindowEnd).Error)
		var relayCalls atomic.Int32
		adapter := &managedInstagramRotatingSendAdapter{
			Adapter: channelapi.NewRelayAdapter(
				models.ChannelInstagram,
				nil,
				"synthetic-test-key",
			),
			send: func(
				_ context.Context,
				account *models.ChannelAccount,
				_ channelapi.OutboundMessage,
			) (channelapi.SendResult, error) {
				relayCalls.Add(1)
				generation, ok := managedMetaCurrentCredentialGeneration(
					account.Credentials,
					time.Now().UTC(),
				)
				require.True(t, ok)
				assert.Equal(t, fixture.oauth.ID, generation.OAuthID)
				assert.Equal(t, fixture.webhook.ID, generation.WebhookID)
				rotateManagedInstagramOutboxGeneration(
					t,
					fixture.db,
					fixture.org.ID,
					fixture.account.ID,
					fixture.job.ID,
					fixture.oauth,
					fixture.webhook,
				)
				return channelapi.SendResult{}, &channelapi.ProviderError{
					Provider:  channelapi.RelayProvider,
					Code:      "synthetic_retryable",
					Message:   "synthetic retryable relay failure",
					Retryable: true,
				}
			},
		}
		fixture.worker.channelAdapterFactory = func(
			_ *models.ChannelAccount,
		) (channelapi.Adapter, error) {
			return adapter, nil
		}
		require.NoError(t, fixture.worker.deliverChannelOutboxJob(
			context.Background(),
			fixture.org.ID,
			fixture.job.ID,
			fixture.job.LockedBy,
		))

		assertManagedInstagramDispatchFailureTerminal(t, fixture.db, fixture.org.ID, fixture.job)
		retryID, claimed, err := fixture.worker.claimChannelOutboxJob(
			fixture.org.ID,
			"must-not-relay-manual-n-plus-one",
		)
		require.NoError(t, err)
		assert.False(t, claimed)
		assert.Equal(t, uuid.Nil, retryID)
		assert.EqualValues(t, 1, relayCalls.Load(), "there must be zero relay calls under N+1")
	})

	t.Run("AI message cannot relay under N plus 1", func(t *testing.T) {
		fixture := createManagedMetaAIOutboxFenceFixture(t)
		var relayCalls atomic.Int32
		adapter := &managedInstagramRotatingSendAdapter{
			Adapter: channelapi.NewRelayAdapter(
				models.ChannelInstagram,
				nil,
				"synthetic-test-key",
			),
			send: func(
				_ context.Context,
				account *models.ChannelAccount,
				_ channelapi.OutboundMessage,
			) (channelapi.SendResult, error) {
				relayCalls.Add(1)
				generation, ok := managedMetaCurrentCredentialGeneration(
					account.Credentials,
					time.Now().UTC(),
				)
				require.True(t, ok)
				assert.Equal(t, fixture.oauth.ID, generation.OAuthID)
				assert.Equal(t, fixture.webhook.ID, generation.WebhookID)
				rotateManagedInstagramOutboxGeneration(
					t,
					fixture.db,
					fixture.fixture.Organization.ID,
					fixture.expected.ID,
					fixture.job.ID,
					fixture.oauth,
					fixture.webhook,
				)
				return channelapi.SendResult{}, errors.New(
					"synthetic retryable AI relay transport failure",
				)
			},
		}
		fixture.worker.channelAdapterFactory = func(
			_ *models.ChannelAccount,
		) (channelapi.Adapter, error) {
			return adapter, nil
		}
		require.NoError(t, fixture.worker.deliverChannelOutboxJob(
			context.Background(),
			fixture.fixture.Organization.ID,
			fixture.job.ID,
			fixture.job.LockedBy,
		))

		assertManagedInstagramDispatchFailureTerminal(
			t,
			fixture.db,
			fixture.fixture.Organization.ID,
			fixture.job,
		)
		retryID, claimed, err := fixture.worker.claimChannelOutboxJob(
			fixture.fixture.Organization.ID,
			"must-not-relay-ai-n-plus-one",
		)
		require.NoError(t, err)
		assert.False(t, claimed)
		assert.Equal(t, uuid.Nil, retryID)
		assert.EqualValues(t, 1, relayCalls.Load(), "there must be zero AI relay calls under N+1")
	})
}

func TestStaticInstagramAndMessengerDispatchingErrorsRemainRetryable(t *testing.T) {
	for _, test := range []struct {
		name    string
		channel models.Channel
		config  models.JSONB
	}{
		{
			name:    "static Instagram",
			channel: models.ChannelInstagram,
			config:  models.JSONB{"outbound_enabled": true},
		},
		{
			name:    "managed Messenger compatibility",
			channel: models.ChannelMessenger,
			config: models.JSONB{
				"outbound_enabled":      true,
				"meta_registry_managed": true,
				"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			org := testutil.CreateTestOrganization(t, db)
			account, _, message, job := createChannelOutboxTestFixture(
				t,
				db,
				org.ID,
				"static-dispatch-error-worker",
			)
			account.Channel = test.channel
			account.Config = test.config
			require.NoError(t, db.Model(&models.ChannelAccount{}).Where(
				"id = ? AND organization_id = ?",
				account.ID,
				org.ID,
			).Updates(map[string]any{
				"channel": account.Channel,
				"config":  account.Config,
			}).Error)
			require.NoError(t, db.Model(&models.OutboxJob{}).Where(
				"id = ? AND organization_id = ?",
				job.ID,
				org.ID,
			).Update("status", models.OutboxJobStatusDispatching).Error)
			job.Status = models.OutboxJobStatusDispatching
			worker := &Worker{DB: db, Log: testutil.NopLogger()}
			require.NoError(t, worker.failChannelOutboxProviderAttempt(
				org.ID,
				job,
				account,
				job.LockedBy,
				errors.New("synthetic retryable static failure"),
				true,
			))
			require.NoError(t, db.First(job, "id = ?", job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusRetrying, job.Status)
			require.NoError(t, db.First(message, "id = ?", message.ID).Error)
			assert.Equal(t, models.MessageStatusPending, message.Status)
		})
	}
}

func updateManagedMetaOutboxURLConfig(
	t *testing.T,
	fixture *managedMetaOutboxFenceFixture,
	key, value string,
) {
	t.Helper()
	accountConfig := cloneChannelOutboxTestJSONB(fixture.account.Config)
	accountConfig[key] = value
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
		Update("config", accountConfig).Error)
}

func TestManagedMetaManualPartialMarkerFailsAndSettlesBeforeProvider(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(models.JSONB)
	}{
		{
			name: "registry marker only",
			mutate: func(accountConfig models.JSONB) {
				delete(accountConfig, "meta_management_mode")
			},
		},
		{
			name: "platform mode only",
			mutate: func(accountConfig models.JSONB) {
				accountConfig["meta_registry_managed"] = false
			},
		},
		{
			name: "both markers stripped with managed residue",
			mutate: func(accountConfig models.JSONB) {
				delete(accountConfig, "meta_registry_managed")
				delete(accountConfig, "meta_management_mode")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := createManagedMetaOutboxFenceFixture(t)
			accountConfig := cloneChannelOutboxTestJSONB(fixture.account.Config)
			test.mutate(accountConfig)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
				Update("config", accountConfig).Error)
			fixture.account.Config = accountConfig
			require.True(t, isManagedMetaChannelOutboxAccount(fixture.account))
			require.False(t, exactManagedMetaChannelOutboxBinding(fixture.account))

			providerCalls := 0
			_, fenceErr := fixture.worker.markManagedMetaChannelOutboxDispatching(
				fixture.org.ID,
				fixture.job.ID,
				fixture.job.LockedBy,
				fixture.account,
			)
			if fenceErr == nil {
				providerCalls++
			}
			require.ErrorIs(t, fenceErr, errChannelOutboxManagedMetaFence)
			require.NoError(t, fixture.worker.failManagedMetaChannelOutboxFence(
				fixture.org.ID,
				fixture.job,
				fixture.job.LockedBy,
				fenceErr,
			))
			assert.Zero(t, providerCalls)
			require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusFailed, fixture.job.Status)
			require.NotNil(t, fixture.job.MessageID)
			var message models.Message
			require.NoError(t, fixture.db.First(&message, "id = ?", *fixture.job.MessageID).Error)
			assert.Equal(t, models.MessageStatusFailed, message.Status)
		})
	}
}

func TestManagedMetaManualDispatchFenceWaitsForCommittedDowngrade(t *testing.T) {
	fixture := createManagedMetaOutboxFenceFixture(t)
	controlTx := fixture.db.Begin()
	require.NoError(t, controlTx.Error)
	t.Cleanup(func() { _ = controlTx.Rollback().Error })
	require.NoError(t, lockChannelOutboxOrganizationScopeTx(controlTx, fixture.org.ID))

	config := cloneChannelOutboxTestJSONB(fixture.account.Config)
	config["outbound_enabled"] = false
	require.NoError(t, controlTx.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusDegraded,
			"config": config,
		}).Error)
	now := time.Now().UTC()
	require.NoError(t, controlTx.Model(&models.OutboxJob{}).
		Where(
			"id = ? AND organization_id = ? AND status IN ?",
			fixture.job.ID,
			fixture.org.ID,
			[]models.OutboxJobStatus{
				models.OutboxJobStatusPending,
				models.OutboxJobStatusRetrying,
				models.OutboxJobStatusProcessing,
			},
		).
		Updates(map[string]any{
			"status":     models.OutboxJobStatusCancelled,
			"failed_at":  now,
			"locked_at":  nil,
			"locked_by":  "",
			"updated_at": now,
		}).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				return err
			}
			fencePID <- backendPID
			worker := &Worker{DB: connection, Config: fixture.worker.Config, Log: testutil.NopLogger()}
			_, err := worker.markManagedMetaChannelOutboxDispatching(
				fixture.org.ID,
				fixture.job.ID,
				fixture.job.LockedBy,
				fixture.account,
			)
			return err
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, controlTx.Commit().Error)

	select {
	case err := <-fenceDone:
		assert.ErrorIs(t, err, errChannelOutboxManagedMetaFence)
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed Meta dispatch did not resume after downgrade committed")
	}
	require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, fixture.job.Status)
}

func TestManagedMetaManualDispatchWinnerCannotBeCancelled(t *testing.T) {
	fixture := createManagedMetaOutboxFenceFixture(t)
	_, err := fixture.worker.markManagedMetaChannelOutboxDispatching(
		fixture.org.ID,
		fixture.job.ID,
		fixture.job.LockedBy,
		fixture.account,
	)
	require.NoError(t, err)

	result := fixture.db.Model(&models.OutboxJob{}).
		Where(
			"id = ? AND organization_id = ? AND status IN ?",
			fixture.job.ID,
			fixture.org.ID,
			[]models.OutboxJobStatus{
				models.OutboxJobStatusPending,
				models.OutboxJobStatusRetrying,
				models.OutboxJobStatusProcessing,
			},
		).
		Update("status", models.OutboxJobStatusCancelled)
	require.NoError(t, result.Error)
	assert.Zero(t, result.RowsAffected)
	require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, fixture.job.Status)
}

func TestChannelAIOutboxDispatchWaitsForTenantControlMutation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"ai-tenant-fence-worker",
	)

	controlTx := db.Begin()
	require.NoError(t, controlTx.Error)
	t.Cleanup(func() { _ = controlTx.Rollback().Error })
	require.NoError(t, lockChannelOutboxOrganizationScopeTx(controlTx, fixture.Organization.ID))
	config := cloneChannelOutboxTestJSONB(fixture.Account.Config)
	config["ai_reply_enabled"] = false
	require.NoError(t, controlTx.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.Account.ID, fixture.Organization.ID).
		Update("config", config).Error)
	require.NoError(t, controlTx.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ? AND status = ?", job.ID, fixture.Organization.ID, models.OutboxJobStatusProcessing).
		Updates(map[string]any{
			"status":    models.OutboxJobStatusCancelled,
			"locked_at": nil,
			"locked_by": "",
		}).Error)

	fencePID := make(chan int, 1)
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
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
	require.NoError(t, controlTx.Commit().Error)
	select {
	case err := <-fenceDone:
		assert.True(t, errors.Is(err, errChannelOutboxAIPolicy), err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "AI dispatch did not resume after tenant control mutation committed")
	}
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, job.Status)
}

func TestManagedMetaAIOutboxDispatchUsesRefreshedSameGenerationCredential(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	profileID := nextManagedMetaOutboxProfileID()
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"managed-ai-refresh-worker",
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", fixture.Organization.ID, fixture.Account.ID).
		Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
		}).Error)
	oauthID := uuid.New()
	webhookID := uuid.New()
	expiresAt := now.Add(2 * time.Hour)
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: oauthID}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{"access_token": "old-same-generation-token"},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "test:v1",
		ExpiresAt: &expiresAt, Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: webhookID}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  "synthetic-encrypted-inbound",
			"outbound_secret": "synthetic-encrypted-outbound",
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "test:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	metadata := models.JSONB{
		"meta_ownership_state":                    metaregistry.OwnershipVerified,
		"meta_ownership_checked_at":               now.Add(-time.Minute).Format(time.RFC3339Nano),
		"meta_platform_app_id":                    managedMetaOutboxTestAppID,
		"meta_webhook_app":                        "instagram_login",
		"meta_authorizing_user_id":                profileID,
		"meta_oauth_subject_id":                   profileID,
		"meta_authority_asset_id":                 profileID,
		"meta_authorization_token_kind":           "USER",
		"meta_authorized_at":                      now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		"meta_granted_scopes":                     []string{"instagram_business_basic", "instagram_business_manage_messages"},
		"meta_release_evidence_mode":              "app_review_approved",
		"meta_release_review_status":              "approved",
		"meta_subscription_desired_state":         "subscribed",
		"meta_subscription_operation_id":          uuid.NewString(),
		"meta_subscription_operation_state":       "subscribe_complete",
		"meta_subscription_operation_expires_at":  now.Add(time.Hour).Format(time.RFC3339Nano),
		"meta_subscription_oauth_credential_id":   oauth.ID.String(),
		"meta_subscription_oauth_version":         oauth.Version,
		"meta_subscription_webhook_credential_id": webhook.ID.String(),
		"meta_subscription_webhook_version":       webhook.Version,
		"meta_subscription_remote_state":          "subscribed",
	}
	accountConfig := models.JSONB{
		"meta_registry_managed": true,
		"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
		"instagram_api_mode":    "instagram_login",
		"rereply_webhook_url":   "https://app.example.test/api/webhooks/channels/" + fixture.Account.ID.String(),
		"relay_url":             "https://relay.example.test/v1/accounts/instagram/" + profileID,
		"outbound_enabled":      true,
		"ai_reply_enabled":      true,
	}
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.Account.ID, fixture.Organization.ID).
		Updates(map[string]any{
			"external_account_id": profileID,
			"config":              accountConfig,
			"metadata":            metadata,
		}).Error)
	expected := *fixture.Account
	expected.ExternalAccountID = profileID
	expected.Config = accountConfig
	expected.Metadata = metadata
	expected.Credentials = []models.ChannelCredential{oauth, webhook}
	worker := &Worker{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{Environment: "production"},
			MetaRegistry: config.MetaRegistryConfig{
				Enabled: true, OwnershipMaxAgeMins: 24 * 60,
			},
			MetaInstagram: config.MetaInstagramConfig{
				Enabled: true, AppID: managedMetaOutboxTestAppID,
				AppReviewStatus: "approved", AllowedOrganizationID: fixture.Organization.ID.String(),
				ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://relay.example.test",
			},
		},
	}

	refreshTx := db.Begin()
	require.NoError(t, refreshTx.Error)
	t.Cleanup(func() { _ = refreshTx.Rollback().Error })
	require.NoError(t, lockChannelOutboxOrganizationScopeTx(refreshTx, fixture.Organization.ID))
	var lockedAccount models.ChannelAccount
	require.NoError(t, refreshTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", fixture.Account.ID, fixture.Organization.ID).
		First(&lockedAccount).Error)
	refreshedBlob := cloneChannelOutboxTestJSONB(oauth.CredentialBlob)
	refreshedBlob["access_token"] = "fresh-same-generation-token"
	require.NoError(t, refreshTx.Model(&models.ChannelCredential{}).
		Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND version = ?",
			oauth.ID,
			fixture.Organization.ID,
			fixture.Account.ID,
			oauth.Version,
		).
		Update("credential_blob", refreshedBlob).Error)

	type fenceResult struct {
		account *models.ChannelAccount
		err     error
	}
	fencePID := make(chan int, 1)
	fenceDone := make(chan fenceResult, 1)
	go func() {
		_ = db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				fencePID <- 0
				fenceDone <- fenceResult{err: err}
				return nil
			}
			fencePID <- backendPID
			threadWorker := *worker
			threadWorker.DB = connection
			account, err := threadWorker.recheckChannelAIOutboxDispatchWithAccount(
				fixture.Organization.ID,
				job.ID,
				job.LockedBy,
				&expected,
			)
			fenceDone <- fenceResult{account: account, err: err}
			return nil
		})
	}()
	backendPID := <-fencePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, db, backendPID)
	require.NoError(t, refreshTx.Commit().Error)

	select {
	case result := <-fenceDone:
		require.NoError(t, result.err)
		require.NotNil(t, result.account)
		var fencedOAuth *models.ChannelCredential
		for index := range result.account.Credentials {
			if result.account.Credentials[index].Kind == models.ChannelCredentialKindOAuth {
				fencedOAuth = &result.account.Credentials[index]
			}
		}
		require.NotNil(t, fencedOAuth)
		assert.Equal(t, oauth.ID, fencedOAuth.ID)
		assert.Equal(t, oauth.Version, fencedOAuth.Version)
		assert.Equal(t, "fresh-same-generation-token", fencedOAuth.CredentialBlob["access_token"])
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed AI dispatch did not resume after credential refresh committed")
	}
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, job.Status)
}

func TestManagedMetaAIOutboxDispatchRejectsFreshGenerationAndURLDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *managedMetaAIOutboxFenceFixture)
	}{
		{
			name: "missing OAuth expiry",
			mutate: func(t *testing.T, fixture *managedMetaAIOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.fixture.Organization.ID).
					Update("expires_at", nil).Error)
			},
		},
		{
			name: "near OAuth expiry",
			mutate: func(t *testing.T, fixture *managedMetaAIOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.fixture.Organization.ID).
					Update("expires_at", time.Now().UTC().Add(time.Minute)).Error)
			},
		},
		{
			name: "future OAuth creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaAIOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.fixture.Organization.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "future webhook creation timestamp",
			mutate: func(t *testing.T, fixture *managedMetaAIOutboxFenceFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.webhook.ID, fixture.fixture.Organization.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "adversarial relay URL",
			mutate: func(t *testing.T, fixture *managedMetaAIOutboxFenceFixture) {
				accountConfig := cloneChannelOutboxTestJSONB(fixture.expected.Config)
				accountConfig["relay_url"] = "https://relay.example.test/v1/accounts/instagram/" +
					fixture.expected.ExternalAccountID + "/suffix"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where(
						"id = ? AND organization_id = ?",
						fixture.expected.ID,
						fixture.fixture.Organization.ID,
					).
					Update("config", accountConfig).Error)
			},
		},
	} {
		t.Run(test.name+" fails before dispatch", func(t *testing.T) {
			fixture := createManagedMetaAIOutboxFenceFixture(t)
			test.mutate(t, &fixture)
			_, err := fixture.worker.recheckChannelAIOutboxDispatchWithAccount(
				fixture.fixture.Organization.ID,
				fixture.job.ID,
				fixture.job.LockedBy,
				&fixture.expected,
			)
			assert.ErrorIs(t, err, errChannelOutboxAIPolicy)
			require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusProcessing, fixture.job.Status)
			assert.Empty(t, fixture.job.ProviderState)
		})
	}
}

func TestManagedMetaAIPartialMarkerCancelsBeforeProvider(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(models.JSONB)
	}{
		{
			name: "registry marker only",
			mutate: func(accountConfig models.JSONB) {
				delete(accountConfig, "meta_management_mode")
			},
		},
		{
			name: "platform mode only",
			mutate: func(accountConfig models.JSONB) {
				accountConfig["meta_registry_managed"] = false
			},
		},
		{
			name: "both markers stripped with managed residue",
			mutate: func(accountConfig models.JSONB) {
				delete(accountConfig, "meta_registry_managed")
				delete(accountConfig, "meta_management_mode")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := createManagedMetaAIOutboxFenceFixture(t)
			accountConfig := cloneChannelOutboxTestJSONB(fixture.expected.Config)
			test.mutate(accountConfig)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where(
					"id = ? AND organization_id = ?",
					fixture.expected.ID,
					fixture.fixture.Organization.ID,
				).
				Update("config", accountConfig).Error)
			fixture.expected.Config = accountConfig
			require.True(t, isManagedMetaChannelOutboxAccount(&fixture.expected))
			require.False(t, exactManagedMetaChannelOutboxBinding(&fixture.expected))

			providerCalls := 0
			_, fenceErr := fixture.worker.recheckChannelAIOutboxDispatchWithAccount(
				fixture.fixture.Organization.ID,
				fixture.job.ID,
				fixture.job.LockedBy,
				&fixture.expected,
			)
			if fenceErr == nil {
				providerCalls++
			}
			require.ErrorIs(t, fenceErr, errChannelOutboxAIPolicy)
			require.NoError(t, fixture.worker.cancelChannelAIOutboxJob(
				fixture.fixture.Organization.ID,
				fixture.job,
				fixture.job.LockedBy,
				fenceErr,
			))
			assert.Zero(t, providerCalls)
			require.NoError(t, fixture.db.First(fixture.job, "id = ?", fixture.job.ID).Error)
			assert.Equal(t, models.OutboxJobStatusCancelled, fixture.job.Status)
			require.NotNil(t, fixture.job.MessageID)
			var message models.Message
			require.NoError(t, fixture.db.First(&message, "id = ?", *fixture.job.MessageID).Error)
			assert.Equal(t, models.MessageStatusFailed, message.Status)
		})
	}
}

func createManagedMetaAIOutboxFenceFixture(t *testing.T) managedMetaAIOutboxFenceFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	fixture := createChannelAIReplyWorkerFixture(t, db)
	profileID := nextManagedMetaOutboxProfileID()
	job, _ := createChannelAIOutboxDispatchFixture(
		t,
		db,
		fixture,
		time.Now().UTC().Add(time.Hour),
		"managed-ai-runtime-worker",
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where(
			"organization_id = ? AND channel_account_id = ?",
			fixture.Organization.ID,
			fixture.Account.ID,
		).
		Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
		}).Error)
	oauthID := uuid.New()
	webhookID := uuid.New()
	expiresAt := now.Add(2 * time.Hour)
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: oauthID}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{"access_token": "synthetic-managed-ai-token"},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "test:v1",
		ExpiresAt: &expiresAt, Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: webhookID}, OrganizationID: fixture.Organization.ID,
		ChannelAccountID: fixture.Account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  "synthetic-encrypted-inbound",
			"outbound_secret": "synthetic-encrypted-outbound",
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "test:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	metadata := models.JSONB{
		"meta_ownership_state":                    metaregistry.OwnershipVerified,
		"meta_ownership_checked_at":               now.Add(-time.Minute).Format(time.RFC3339Nano),
		"meta_platform_app_id":                    managedMetaOutboxTestAppID,
		"meta_webhook_app":                        "instagram_login",
		"meta_authorizing_user_id":                profileID,
		"meta_oauth_subject_id":                   profileID,
		"meta_authority_asset_id":                 profileID,
		"meta_authorization_token_kind":           "USER",
		"meta_authorized_at":                      now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		"meta_granted_scopes":                     []string{"instagram_business_basic", "instagram_business_manage_messages"},
		"meta_release_evidence_mode":              "app_review_approved",
		"meta_release_review_status":              "approved",
		"meta_subscription_desired_state":         "subscribed",
		"meta_subscription_operation_id":          uuid.NewString(),
		"meta_subscription_operation_state":       "subscribe_complete",
		"meta_subscription_operation_expires_at":  now.Add(time.Hour).Format(time.RFC3339Nano),
		"meta_subscription_oauth_credential_id":   oauth.ID.String(),
		"meta_subscription_oauth_version":         oauth.Version,
		"meta_subscription_webhook_credential_id": webhook.ID.String(),
		"meta_subscription_webhook_version":       webhook.Version,
		"meta_subscription_remote_state":          "subscribed",
	}
	accountConfig := models.JSONB{
		"meta_registry_managed": true,
		"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
		"instagram_api_mode":    "instagram_login",
		"rereply_webhook_url": "https://app.example.test/api/webhooks/channels/" +
			fixture.Account.ID.String(),
		"relay_url": "https://relay.example.test/v1/accounts/instagram/" +
			profileID,
		"outbound_enabled": true,
		"ai_reply_enabled": true,
	}
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.Account.ID, fixture.Organization.ID).
		Updates(map[string]any{
			"external_account_id": profileID,
			"config":              accountConfig,
			"metadata":            metadata,
		}).Error)
	expected := *fixture.Account
	expected.ExternalAccountID = profileID
	expected.Config = accountConfig
	expected.Metadata = metadata
	expected.Credentials = []models.ChannelCredential{oauth, webhook}
	worker := &Worker{
		DB: db, Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{Environment: "production"},
			MetaRegistry: config.MetaRegistryConfig{
				Enabled: true, OwnershipMaxAgeMins: 24 * 60,
			},
			MetaInstagram: config.MetaInstagramConfig{
				Enabled: true, AppID: managedMetaOutboxTestAppID,
				AppReviewStatus: "approved", AllowedOrganizationID: fixture.Organization.ID.String(),
				ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://relay.example.test",
			},
		},
	}
	return managedMetaAIOutboxFenceFixture{
		db: db, fixture: fixture, job: job, oauth: oauth, webhook: webhook,
		expected: expected, worker: worker,
	}
}

func rotateManagedInstagramOutboxGeneration(
	t *testing.T,
	db *gorm.DB,
	organizationID, accountID, jobID uuid.UUID,
	oauth, webhook models.ChannelCredential,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelOutboxOrganizationScopeTx(tx, organizationID); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			accountID,
			organizationID,
		).First(&account).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ?",
			organizationID,
			accountID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		).Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
		}).Error; err != nil {
			return err
		}
		expiresAt := now.Add(2 * time.Hour)
		rotatedOAuth := oauth
		rotatedOAuth.BaseModel = models.BaseModel{ID: uuid.New()}
		rotatedOAuth.Version++
		rotatedOAuth.Status = models.ChannelCredentialStatusActive
		rotatedOAuth.RevokedAt = nil
		rotatedOAuth.ExpiresAt = &expiresAt
		rotatedOAuth.CredentialBlob = models.JSONB{"access_token": "synthetic-rotated-dispatch-token"}
		rotatedWebhook := webhook
		rotatedWebhook.BaseModel = models.BaseModel{ID: uuid.New()}
		rotatedWebhook.Version++
		rotatedWebhook.Status = models.ChannelCredentialStatusActive
		rotatedWebhook.RevokedAt = nil
		rotatedWebhook.CredentialBlob = models.JSONB{
			"inbound_secret":  "synthetic-rotated-dispatch-inbound",
			"outbound_secret": "synthetic-rotated-dispatch-outbound",
		}
		if err := tx.Create(&rotatedOAuth).Error; err != nil {
			return err
		}
		if err := tx.Create(&rotatedWebhook).Error; err != nil {
			return err
		}
		metadata := cloneChannelOutboxTestJSONB(account.Metadata)
		metadata["meta_subscription_operation_id"] = uuid.NewString()
		metadata["meta_subscription_operation_expires_at"] = now.Add(time.Hour).Format(time.RFC3339Nano)
		metadata["meta_subscription_oauth_credential_id"] = rotatedOAuth.ID.String()
		metadata["meta_subscription_oauth_version"] = rotatedOAuth.Version
		metadata["meta_subscription_webhook_credential_id"] = rotatedWebhook.ID.String()
		metadata["meta_subscription_webhook_version"] = rotatedWebhook.Version
		if err := tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?",
			accountID,
			organizationID,
		).Update("metadata", metadata).Error; err != nil {
			return err
		}
		cancelled := tx.Model(&models.OutboxJob{}).Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND status IN ?",
			jobID,
			organizationID,
			accountID,
			[]models.OutboxJobStatus{
				models.OutboxJobStatusPending,
				models.OutboxJobStatusRetrying,
				models.OutboxJobStatusProcessing,
			},
		).Update("status", models.OutboxJobStatusCancelled)
		if cancelled.Error != nil {
			return cancelled.Error
		}
		require.Zero(t, cancelled.RowsAffected, "dispatching is the relay attempt point of no return")
		return nil
	}))
}

func assertManagedInstagramDispatchFailureTerminal(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	job *models.OutboxJob,
) {
	t.Helper()
	require.NoError(t, db.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, models.OutboxJobStatusFailed, job.Status)
	assert.Equal(t, "managed_instagram_delivery_state_ambiguous", job.LastErrorCode)
	assert.NotNil(t, job.FailedAt)
	require.NotNil(t, job.MessageID)
	var message models.Message
	require.NoError(t, db.First(&message, "id = ?", *job.MessageID).Error)
	assert.Equal(t, models.MessageStatusFailed, message.Status)
	var eventCount int64
	require.NoError(t, db.Model(&models.MessageEvent{}).Where(
		"organization_id = ? AND channel_account_id = ? AND message_id = ? AND error_code = ?",
		organizationID,
		job.ChannelAccountID,
		*job.MessageID,
		"managed_instagram_delivery_state_ambiguous",
	).Count(&eventCount).Error)
	assert.EqualValues(t, 1, eventCount)
}

func createManagedMetaOutboxFenceFixture(t *testing.T) managedMetaOutboxFenceFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	account, _, _, job := createChannelOutboxTestFixture(t, db, org.ID, "managed-meta-fence-worker")
	profileID := nextManagedMetaOutboxProfileID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	oauthID := uuid.New()
	webhookID := uuid.New()
	operationID := uuid.New()
	metadata := models.JSONB{
		"meta_ownership_state":                    metaregistry.OwnershipVerified,
		"meta_ownership_checked_at":               now.Add(-time.Minute).Format(time.RFC3339Nano),
		"meta_platform_app_id":                    managedMetaOutboxTestAppID,
		"meta_webhook_app":                        "instagram_login",
		"meta_authorizing_user_id":                profileID,
		"meta_oauth_subject_id":                   profileID,
		"meta_authority_asset_id":                 profileID,
		"meta_authorization_token_kind":           "USER",
		"meta_authorized_at":                      now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		"meta_granted_scopes":                     []string{"instagram_business_basic", "instagram_business_manage_messages"},
		"meta_release_evidence_mode":              "app_review_approved",
		"meta_release_review_status":              "approved",
		"meta_subscription_desired_state":         "subscribed",
		"meta_subscription_operation_id":          operationID.String(),
		"meta_subscription_operation_state":       "subscribe_complete",
		"meta_subscription_operation_expires_at":  now.Add(time.Hour).Format(time.RFC3339Nano),
		"meta_subscription_oauth_credential_id":   oauthID.String(),
		"meta_subscription_oauth_version":         1,
		"meta_subscription_webhook_credential_id": webhookID.String(),
		"meta_subscription_webhook_version":       1,
		"meta_subscription_remote_state":          "subscribed",
	}
	account.Channel = models.ChannelInstagram
	account.Provider = channelapi.RelayProvider
	account.ExternalAccountID = profileID
	account.Status = models.ChannelAccountStatusActive
	account.Config = models.JSONB{
		"meta_registry_managed": true,
		"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
		"instagram_api_mode":    "instagram_login",
		"rereply_webhook_url":   "https://app.example.test/api/webhooks/channels/" + account.ID.String(),
		"relay_url":             "https://relay.example.test/v1/accounts/instagram/" + profileID,
		"outbound_enabled":      true,
	}
	account.Metadata = metadata
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Updates(map[string]any{
			"channel":             account.Channel,
			"provider":            account.Provider,
			"external_account_id": account.ExternalAccountID,
			"status":              account.Status,
			"config":              account.Config,
			"metadata":            account.Metadata,
		}).Error)
	expiresAt := now.Add(2 * time.Hour)
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: oauthID}, OrganizationID: org.ID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{"access_token": "synthetic-encrypted-token"},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "test:v1",
		ExpiresAt: &expiresAt, Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: webhookID}, OrganizationID: org.ID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  "synthetic-encrypted-inbound",
			"outbound_secret": "synthetic-encrypted-outbound",
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "test:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	account.Credentials = []models.ChannelCredential{oauth, webhook}
	worker := &Worker{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{Environment: "production"},
			MetaRegistry: config.MetaRegistryConfig{
				Enabled: true, OwnershipMaxAgeMins: 24 * 60,
			},
			MetaInstagram: config.MetaInstagramConfig{
				Enabled: true, AppID: managedMetaOutboxTestAppID,
				AppReviewStatus: "approved", AllowedOrganizationID: org.ID.String(),
				ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://relay.example.test",
			},
		},
	}
	return managedMetaOutboxFenceFixture{
		db: db, worker: worker, org: org, account: account, job: job,
		oauth: oauth, webhook: webhook,
	}
}
