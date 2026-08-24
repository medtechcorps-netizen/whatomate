package handlers

import (
	"errors"
	"net/http"
	"sync/atomic"
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

func TestManagedInstagramMessageEnqueueRequiresExactCurrentBinding(t *testing.T) {
	t.Run("active exact pair succeeds", func(t *testing.T) {
		fixture := newMetaInstagramLifecycleFixture(t)
		err := fixture.db.Transaction(func(tx *gorm.DB) error {
			if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
				return err
			}
			return lockChannelAccountForMessageEnqueue(
				tx,
				fixture.org.ID,
				fixture.account.ID,
				models.ChannelInstagram,
				time.Now().UTC(),
				fixture.app,
			)
		})
		require.NoError(t, err)
	})

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "ownership downgrade fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_ownership_state"] = "stale"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "runtime quarantine fails closed",
			mutate: func(_ *testing.T, fixture metaInstagramLifecycleFixture) {
				fixture.app.Config.MetaInstagram.QuarantineOnly = true
			},
		},
		{
			name: "registry marker without platform mode and one credential fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				delete(config, "meta_management_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("config", config).Error)
				now := time.Now().UTC()
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Updates(map[string]any{
						"status":     models.ChannelCredentialStatusRevoked,
						"revoked_at": now,
					}).Error)
			},
		},
		{
			name: "platform mode without registry marker and one credential fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				config["meta_registry_managed"] = false
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("config", config).Error)
				now := time.Now().UTC()
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Updates(map[string]any{
						"status":     models.ChannelCredentialStatusRevoked,
						"revoked_at": now,
					}).Error)
			},
		},
		{
			name: "pending deauthorization fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata[metaDeauthorizationPendingDigestKey] = "pending"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "pending data deletion fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata[metaInstagramDeletionPendingDigestKey] = "pending"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "zero OAuth creation timestamp fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("created_at", time.Time{}).Error)
			},
		},
		{
			name: "future OAuth creation timestamp fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "missing OAuth expiry fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("expires_at", nil).Error)
			},
		},
		{
			name: "near OAuth expiry fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("expires_at", time.Now().UTC().Add(time.Minute)).Error)
			},
		},
		{
			name: "zero webhook creation timestamp fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.webhook.ID, fixture.org.ID).
					Update("created_at", time.Time{}).Error)
			},
		},
		{
			name: "future webhook creation timestamp fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.webhook.ID, fixture.org.ID).
					Update("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "webhook URL userinfo fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://attacker@app.example.test/api/webhooks/channels/"+fixture.account.ID.String(),
				)
			},
		},
		{
			name: "webhook URL query fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"?next=https://attacker.example.test",
				)
			},
		},
		{
			name: "webhook URL fragment fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"#suffix",
				)
			},
		},
		{
			name: "webhook URL raw path fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/%63hannels/"+fixture.account.ID.String(),
				)
			},
		},
		{
			name: "webhook URL suffix fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "rereply_webhook_url",
					"https://app.example.test/api/webhooks/channels/"+fixture.account.ID.String()+"/extra",
				)
			},
		},
		{
			name: "relay URL deceptive host fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test.attacker.test/v1/accounts/instagram/"+fixture.profileID,
				)
			},
		},
		{
			name: "relay URL query fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test/v1/accounts/instagram/"+fixture.profileID+"?token=x",
				)
			},
		},
		{
			name: "relay URL suffix fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t, fixture, "relay_url",
					"https://relay.example.test/v1/accounts/instagram/"+fixture.profileID+"/extra",
				)
			},
		},
		{
			name: "authorizing identity mismatch fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_authorizing_user_id"] = "700000000000199"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					Update("metadata", metadata).Error)
			},
		},
		{
			name: "credential rotation without exact subscription binding fails closed",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				now := time.Now().UTC()
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("organization_id = ? AND channel_account_id = ?", fixture.org.ID, fixture.account.ID).
					Updates(map[string]any{
						"status":     models.ChannelCredentialStatusRevoked,
						"revoked_at": now,
					}).Error)
				expiresAt := now.Add(time.Hour)
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
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			test.mutate(t, fixture)
			err := fixture.db.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
					return err
				}
				return lockChannelAccountForMessageEnqueue(
					tx,
					fixture.org.ID,
					fixture.account.ID,
					models.ChannelInstagram,
					time.Now().UTC(),
					fixture.app,
				)
			})
			assert.ErrorIs(t, err, errChannelAccountUnavailableAtEnqueue)

			var messageCount, outboxCount int64
			require.NoError(t, fixture.db.Model(&models.Message{}).
				Where("organization_id = ?", fixture.org.ID).
				Count(&messageCount).Error)
			require.NoError(t, fixture.db.Model(&models.OutboxJob{}).
				Where(
					"organization_id = ? AND channel_account_id = ?",
					fixture.org.ID,
					fixture.account.ID,
				).
				Count(&outboxCount).Error)
			assert.Zero(t, messageCount)
			assert.Zero(t, outboxCount)
		})
	}
}

func updateMetaInstagramOutboundURLConfig(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	key, value string,
) {
	t.Helper()
	config := cloneJSONB(fixture.account.Config)
	config[key] = value
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
		Update("config", config).Error)
}

func TestManagedInstagramReadReceiptPersistsLocallyButRejectsRuntimeDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "exact URL binding drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				updateMetaInstagramOutboundURLConfig(
					t,
					fixture,
					"relay_url",
					"https://relay.example.test/v1/accounts/instagram/"+fixture.profileID+"?drift=1",
				)
			},
		},
		{
			name: "OAuth expiry generation drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
					Update("expires_at", nil).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			require.NotNil(t, fixture.user.RoleID)
			for _, action := range []string{models.ActionRead, models.ActionWrite} {
				var permission models.Permission
				require.NoError(t, fixture.db.Where(
					"resource = ? AND action = ?",
					models.ResourceConversations,
					action,
				).First(&permission).Error)
				require.NoError(t, fixture.db.Create(&models.RolePermission{
					CustomRoleID: *fixture.user.RoleID,
					PermissionID: permission.ID,
				}).Error)
			}

			capabilities := cloneJSONB(fixture.account.Capabilities)
			capabilities[string(channelapi.CapabilityReadReceipts)] = true
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
				Update("capabilities", capabilities).Error)

			contact := models.Contact{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
				PhoneNumber: "ig-read-" + uuid.NewString(), WhatsAppAccount: fixture.account.Name,
				Tags: models.JSONBArray{}, Metadata: models.JSONB{},
			}
			require.NoError(t, fixture.db.Create(&contact).Error)
			conversation := models.InboxConversation{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
				ChannelAccountID: fixture.account.ID, ContactID: contact.ID,
				Channel: models.ChannelInstagram, ExternalConversationID: "ig-read-thread-" + uuid.NewString(),
				Status: models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
				Config: models.JSONB{}, Metadata: models.JSONB{},
			}
			require.NoError(t, fixture.db.Create(&conversation).Error)
			test.mutate(t, fixture)

			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					providerCalls.Add(1)
					return nil, errors.New("provider must not be called after managed Instagram drift")
				},
			)}
			request := testutil.NewJSONRequest(t, MarkInboxConversationReadRequest{
				ExternalMessageIDs: []string{"ig-provider-message-1"},
			})
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			testutil.SetPathParam(request, "id", conversation.ID.String())
			require.NoError(t, fixture.app.MarkInboxConversationRead(request))

			assert.Equal(t, 200, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
			assert.Contains(t, string(request.RequestCtx.Response.Body()), `"provider_synced":false`)
			var readCount int64
			require.NoError(t, fixture.db.Model(&models.ConversationRead{}).
				Where(
					"organization_id = ? AND conversation_id = ? AND reader_key = ?",
					fixture.org.ID,
					conversation.ID,
					"user:"+fixture.user.ID.String(),
				).
				Count(&readCount).Error)
			assert.EqualValues(t, 1, readCount)
		})
	}
}

func TestManagedInstagramMessageEnqueueWaitsForCommittedQuarantine(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	quarantineTx := fixture.db.Begin()
	require.NoError(t, quarantineTx.Error)
	t.Cleanup(func() { _ = quarantineTx.Rollback().Error })
	require.NoError(t, lockChannelAIOrganizationScopeTx(quarantineTx, fixture.org.ID))

	var account models.ChannelAccount
	require.NoError(t, quarantineTx.Where(
		"id = ? AND organization_id = ?",
		fixture.account.ID,
		fixture.org.ID,
	).First(&account).Error)
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	require.NoError(t, quarantineTx.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, fixture.org.ID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusDegraded,
			"config": config,
		}).Error)

	enqueuePID := make(chan int, 1)
	enqueueDone := make(chan error, 1)
	go func() {
		enqueueDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			var backendPID int
			if err := connection.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				enqueuePID <- 0
				return err
			}
			enqueuePID <- backendPID
			return connection.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
					return err
				}
				return lockChannelAccountForMessageEnqueue(
					tx,
					fixture.org.ID,
					fixture.account.ID,
					models.ChannelInstagram,
					time.Now().UTC(),
					fixture.app,
				)
			})
		})
	}()

	backendPID := <-enqueuePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, quarantineTx.Commit().Error)
	select {
	case err := <-enqueueDone:
		assert.True(t, errors.Is(err, errChannelAccountUnavailableAtEnqueue), err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed Instagram enqueue did not resume after quarantine committed")
	}
}
