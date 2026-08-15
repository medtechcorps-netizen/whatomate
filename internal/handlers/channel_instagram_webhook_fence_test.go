package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestManagedInstagramWebhookFenceDoesNotClassifyStaticInstagramRelay(t *testing.T) {
	t.Parallel()
	static := models.ChannelAccount{
		Channel:  models.ChannelInstagram,
		Provider: channelapi.RelayProvider,
		Config: models.JSONB{
			"relay_url":          "https://relay.example.test/static-instagram",
			"instagram_api_mode": "instagram_login",
		},
		Metadata: models.JSONB{"meta_webhook_app": "synthetic_static_app"},
	}
	assert.False(t, managedInstagramLoginWebhookAccount(&static))

	managed := static
	managed.Config = cloneJSONB(static.Config)
	managed.Config["meta_registry_managed"] = true
	managed.Config["meta_management_mode"] = metaregistry.ManagementModePlatformOAuth
	assert.True(t, managedInstagramLoginWebhookAccount(&managed))
}

func TestManagedInstagramWebhookPersistenceWaitsForDowngradeAndFailsClosed(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	body := []byte(`{"external_account_id":"` + fixture.profileID + `","events":[]}`)
	headers := signedManagedInstagramRelayHeaders(body, "synthetic-inbound-secret")

	initialAccount := fixture.account
	initialAccount.Credentials = []models.ChannelCredential{fixture.oauth, fixture.webhook}
	adapter, err := fixture.app.channelAdapter(&initialAccount)
	require.NoError(t, err)
	require.NoError(t, adapter.VerifyWebhook(&initialAccount, headers, body))

	downgradeTx := beginManagedInstagramWebhookDowngrade(
		t,
		fixture,
		"synthetic_concurrent_downgrade",
	)

	webhookPID := make(chan int, 1)
	webhookDone := make(chan error, 1)
	go func() {
		webhookDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				webhookPID <- 0
				return err
			}
			webhookPID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				account, lockErr := fixture.app.lockMetaInstagramWebhookPersistenceBinding(
					tx,
					fixture.org.ID,
					fixture.account.ID,
					fixture.profileID,
					fixture.webhook.ID,
					fixture.webhook.Version,
					headers,
					body,
					time.Now().UTC(),
				)
				if lockErr != nil {
					return lockErr
				}
				rawEvent := models.InboundEvent{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: fixture.org.ID, ChannelAccountID: account.ID,
					DedupeKey: "raw:synthetic-concurrent-downgrade",
					EventType: "raw_webhook", Status: models.InboundEventStatusPending,
					SignatureValid: true, ReceivedAt: time.Now().UTC(),
					Headers: models.JSONB{}, Payload: models.JSONB{"synthetic": true},
				}
				_, _, lockErr = persistOrClaimRawInboundEvent(tx, &rawEvent)
				return lockErr
			})
		})
	}()

	backendPID := <-webhookPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, downgradeTx.Commit().Error)

	select {
	case err := <-webhookDone:
		require.Error(t, err)
		assert.True(t, errors.Is(err, errMetaInstagramWebhookBindingInactive), err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed Instagram webhook persistence did not resume after downgrade committed")
	}

	assertManagedInstagramInboundPersistenceEmpty(t, fixture)
}

func TestManagedInstagramCanonicalPersistenceWaitsForDowngradeAndQuarantinesRaw(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	body := []byte(`{"external_account_id":"` + fixture.profileID + `","events":[]}`)
	headers := signedManagedInstagramRelayHeaders(body, "synthetic-inbound-secret")
	rawEvent := models.InboundEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   fixture.org.ID,
		ChannelAccountID: fixture.account.ID,
		DedupeKey:        "raw:synthetic-second-boundary-downgrade",
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusPending,
		SignatureValid:   true,
		ReceivedAt:       time.Now().UTC(),
		Headers:          models.JSONB{"content-type": "application/json"},
		Payload:          models.JSONB{"synthetic": true},
	}
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		account, err := fixture.app.lockMetaInstagramWebhookPersistenceBinding(
			tx,
			fixture.org.ID,
			fixture.account.ID,
			fixture.profileID,
			fixture.webhook.ID,
			fixture.webhook.Version,
			headers,
			body,
			time.Now().UTC(),
		)
		if err != nil {
			return err
		}
		rawEvent.ChannelAccountID = account.ID
		_, _, err = persistOrClaimRawInboundEvent(tx, &rawEvent)
		return err
	}))

	downgradeTx := beginManagedInstagramWebhookDowngrade(
		t,
		fixture,
		"synthetic_second_boundary_downgrade",
	)
	canonicalPID := make(chan int, 1)
	canonicalDone := make(chan error, 1)
	go func() {
		canonicalDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				canonicalPID <- 0
				return err
			}
			canonicalPID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				account, lockErr := fixture.app.lockMetaInstagramWebhookPersistenceBinding(
					tx,
					fixture.org.ID,
					fixture.account.ID,
					fixture.profileID,
					fixture.webhook.ID,
					fixture.webhook.Version,
					headers,
					body,
					time.Now().UTC(),
				)
				if lockErr != nil {
					return lockErr
				}
				event := validPolicyInboundEvent(time.Now().UTC())
				return processNormalizedChannelEvent(tx, account, &event, rawEvent.ReceivedAt)
			})
		})
	}()

	backendPID := <-canonicalPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, downgradeTx.Commit().Error)

	select {
	case err := <-canonicalDone:
		require.Error(t, err)
		assert.ErrorIs(t, err, errMetaInstagramWebhookBindingInactive)
	case <-time.After(5 * time.Second):
		require.Fail(t, "managed Instagram canonical persistence did not resume after downgrade committed")
	}
	require.NoError(t, markInboundEventIgnored(
		fixture.db,
		fixture.org.ID,
		rawEvent.ID,
		"managed_instagram_binding_inactive",
		"Synthetic managed Instagram binding changed before canonical persistence",
	))

	var storedRaw models.InboundEvent
	require.NoError(t, fixture.db.First(&storedRaw, "id = ?", rawEvent.ID).Error)
	assert.Equal(t, models.InboundEventStatusIgnored, storedRaw.Status)
	assert.Equal(t, "managed_instagram_binding_inactive", storedRaw.ErrorCode)
	assert.Equal(t, true, storedRaw.Payload["redacted"])
	assert.Empty(t, storedRaw.Headers)
	assertManagedInstagramCanonicalPersistenceEmpty(t, fixture)
}

func TestManagedInstagramWebhookPersistenceRejectsSupersededSignatureGeneration(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	body := []byte(`{"external_account_id":"` + fixture.profileID + `","events":[]}`)
	headers := signedManagedInstagramRelayHeaders(body, "synthetic-inbound-secret")

	replacement := fixture.webhook
	replacement.ID = uuid.New()
	replacement.Version++
	replacement.CreatedAt = time.Time{}
	replacement.UpdatedAt = time.Time{}
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
		).First(&models.ChannelAccount{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).Where(
			"id = ? AND organization_id = ?", fixture.webhook.ID, fixture.org.ID,
		).Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&replacement).Error; err != nil {
			return err
		}
		metadata := metadataWithMetaMessengerSubscriptionOperation(
			fixture.account.Metadata,
			metaMessengerSubscriptionOperation{
				ID: uuid.New(), OAuthCredentialID: fixture.oauth.ID, OAuthVersion: fixture.oauth.Version,
				WebhookCredentialID: replacement.ID, WebhookVersion: replacement.Version,
				DesiredState: metaMessengerSubscriptionDesiredSubscribed,
				State:        metaMessengerSubscriptionSubscribeComplete,
				ExpiresAt:    time.Now().UTC().Add(time.Hour),
			},
			metaMessengerSubscriptionRemoteSubscribed,
		)
		return tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
		).Update("metadata", metadata).Error
	}))

	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, lockErr := fixture.app.lockMetaInstagramWebhookPersistenceBinding(
			tx,
			fixture.org.ID,
			fixture.account.ID,
			fixture.profileID,
			fixture.webhook.ID,
			fixture.webhook.Version,
			headers,
			body,
			time.Now().UTC(),
		)
		return lockErr
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, errMetaInstagramWebhookBindingInactive)
	assertManagedInstagramInboundPersistenceEmpty(t, fixture)
}

func signedManagedInstagramRelayHeaders(body []byte, secret string) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	headers := make(http.Header)
	headers.Set(channelapi.RelaySignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return headers
}

func beginManagedInstagramWebhookDowngrade(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	reason string,
) *gorm.DB {
	t.Helper()
	downgradeTx := fixture.db.Begin()
	require.NoError(t, downgradeTx.Error)
	t.Cleanup(func() { _ = downgradeTx.Rollback().Error })
	require.NoError(t, lockChannelAIOrganizationScopeTx(downgradeTx, fixture.org.ID))
	var lockedAccount models.ChannelAccount
	require.NoError(t, downgradeTx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).First(&lockedAccount).Error)
	metadata := cloneJSONB(lockedAccount.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_reason"] = reason
	metadata["meta_ownership_checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	metadata["meta_activation_state"] = "quarantined"
	config := cloneJSONB(lockedAccount.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	require.NoError(t, downgradeTx.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).Updates(map[string]any{
		"status":   models.ChannelAccountStatusDegraded,
		"config":   config,
		"metadata": metadata,
	}).Error)
	return downgradeTx
}

func assertManagedInstagramInboundPersistenceEmpty(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) {
	t.Helper()
	for name, model := range map[string]any{
		"raw inbound event":  &models.InboundEvent{},
		"inbox conversation": &models.InboxConversation{},
		"message":            &models.Message{},
		"AI scheduled job":   &models.ScheduledJob{},
	} {
		var count int64
		require.NoError(t, fixture.db.Model(model).Where(
			"organization_id = ?", fixture.org.ID,
		).Count(&count).Error, name)
		assert.Zero(t, count, name)
	}
}

func assertManagedInstagramCanonicalPersistenceEmpty(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) {
	t.Helper()
	for name, model := range map[string]any{
		"canonical inbound event": &models.InboundEvent{},
		"contact":                 &models.Contact{},
		"contact identity":        &models.ContactIdentity{},
		"inbox conversation":      &models.InboxConversation{},
		"message":                 &models.Message{},
		"AI scheduled job":        &models.ScheduledJob{},
	} {
		var count int64
		query := fixture.db.Model(model).Where("organization_id = ?", fixture.org.ID)
		if name == "canonical inbound event" {
			query = query.Where("event_type <> ?", "raw_webhook")
		}
		require.NoError(t, query.Count(&count).Error, name)
		assert.Zero(t, count, name)
	}
}
