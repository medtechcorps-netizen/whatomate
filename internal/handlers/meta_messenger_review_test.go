package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

const (
	reviewHandlerTestAppID                     = "1035383549213572"
	reviewHandlerTestOwnerBusinessID           = "2018290039073161"
	reviewHandlerTestBusinessID                = "3852210034910979"
	reviewHandlerTestEncryptionKey             = "review-handler-encryption-key-at-least-32-bytes"
	reviewHandlerTestBrokerAuthSecret          = "review-handler-broker-auth-root-at-least-32-bytes"
	reviewHandlerTestBrokerWrapSecret          = "review-handler-broker-wrap-root-at-least-32-bytes"
	reviewHandlerTestProviderProof             = "review-handler-provider-proof-root-at-least-32-bytes"
	reviewHandlerTestInboundSecret             = "review-handler-inbound-secret-at-least-32-bytes"
	reviewHandlerTestRelayBaseURL              = "https://review-relay.example.test"
	reviewHandlerTestReReplyBaseURL            = "https://staging.rereply.example.test"
	reviewHandlerTestDeprovisionedError        = "staging_review_deprovisioned"
	reviewHandlerTestPageIDBase         uint64 = 1038752885977372
)

var reviewHandlerTestMetaIDSequence atomic.Uint64

type reviewHandlerFixture struct {
	app        *App
	accountID  uuid.UUID
	orgID      uuid.UUID
	generation uuid.UUID
	tuple      metareview.ProvisionTuple
}

func newReviewHandlerFixture(t *testing.T) reviewHandlerFixture {
	t.Helper()
	now := time.Now().UTC()
	orgID := uuid.New()
	accountID := uuid.New()
	generation := uuid.New()
	pageID := strconv.FormatUint(
		reviewHandlerTestPageIDBase+reviewHandlerTestMetaIDSequence.Add(1),
		10,
	)
	app := &App{Config: &configpkg.Config{
		App: configpkg.AppConfig{
			Environment:   "staging",
			EncryptionKey: reviewHandlerTestEncryptionKey,
		},
		MetaMessengerOnboarding: configpkg.MetaMessengerOnboardingConfig{
			Enabled:             true,
			AppID:               reviewHandlerTestAppID,
			ConfigID:            "1720929458946813",
			OwnerBusinessID:     reviewHandlerTestOwnerBusinessID,
			AppSecret:           "review-handler-meta-app-secret-at-least-32-bytes",
			GraphAPIVersion:     "v25.0",
			GraphBaseURL:        "https://graph.facebook.com",
			TrustedRelayBaseURL: reviewHandlerTestRelayBaseURL,
		},
		MetaMessengerReviewRelay: configpkg.MetaMessengerReviewRelayConfig{
			Enabled:                 true,
			Mode:                    metareview.Mode,
			OrganizationID:          orgID.String(),
			MetaBusinessID:          reviewHandlerTestBusinessID,
			PageID:                  pageID,
			ChannelAccountID:        accountID.String(),
			ReviewerOutboundEnabled: true,
			ReviewerUserID:          uuid.NewString(),
			ReviewerRoleID:          uuid.NewString(),
			Generation:              generation.String(),
			ExpiresAt:               now.Add(2 * time.Hour).Format(time.RFC3339Nano),
			RelayBaseURL:            reviewHandlerTestRelayBaseURL,
			ReReplyBaseURL:          reviewHandlerTestReReplyBaseURL,
			BrokerAuthSecret:        reviewHandlerTestBrokerAuthSecret,
			BrokerWrapSecret:        reviewHandlerTestBrokerWrapSecret,
			ProviderProofSecret:     reviewHandlerTestProviderProof,
		},
	}}
	app.Log = testutil.NopLogger()
	_, tuple, err := app.metaMessengerReviewSettings(now)
	require.NoError(t, err)
	return reviewHandlerFixture{
		app:        app,
		accountID:  accountID,
		orgID:      orgID,
		generation: generation,
		tuple:      tuple,
	}
}

func (fixture reviewHandlerFixture) readyAccount(t *testing.T) models.ChannelAccount {
	t.Helper()
	relayURL, err := metaMessengerRelayURL(
		fixture.app.Config.MetaMessengerReviewRelay.RelayBaseURL,
		fixture.tuple.PageID,
	)
	require.NoError(t, err)
	return models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: fixture.accountID},
		OrganizationID:    fixture.orgID,
		Channel:           models.ChannelMessenger,
		Provider:          channelapi.RelayProvider,
		Name:              "Staging Messenger review",
		ExternalAccountID: fixture.tuple.PageID,
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      models.JSONB{"text": true, "service_window": true},
		Config: models.JSONB{
			"relay_url":        relayURL,
			"outbound_enabled": false,
			"ai_reply_enabled": false,
		},
		Metadata: models.JSONB{
			"management_mode":            metaMessengerManagementMode,
			"review_relay_mode":          metareview.Marker,
			"review_generation":          fixture.generation.String(),
			"review_expires_at":          fixture.tuple.ExpiresAt,
			"meta_business_id":           fixture.tuple.MetaBusinessID,
			"meta_app_id":                fixture.tuple.MetaAppID,
			"meta_app_owner_business_id": reviewHandlerTestOwnerBusinessID,
			"review_ready":               true,
			"subscription_verified":      true,
		},
		IsDefaultIncoming: false,
		IsDefaultOutgoing: false,
	}
}

func TestMetaMessengerReviewBrokerAuthenticatesBeforeRedisOrTenantStorage(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	now := time.Now().UTC()
	messengerAppSecretKeyID, err := metareview.MessengerAppSecretKeyID(
		fixture.app.Config.MetaMessengerOnboarding.AppSecret,
	)
	require.NoError(t, err)
	providerProofSecretKeyID, err := metareview.ProviderProofSecretKeyID(
		fixture.app.Config.MetaMessengerReviewRelay.ProviderProofSecret,
	)
	require.NoError(t, err)
	body, err := metareview.EncodeProvisionRequest(metareview.ProvisionRequest{
		Version:                  metareview.Version,
		Mode:                     metareview.Mode,
		Tuple:                    fixture.tuple,
		MessengerAppSecretKeyID:  messengerAppSecretKeyID,
		ProviderProofSecretKeyID: providerProofSecretKeyID,
	}, now)
	require.NoError(t, err)
	nonce, err := metareview.NewNonce()
	require.NoError(t, err)

	var redisDials atomic.Int32
	fixture.app.Redis = redis.NewClient(&redis.Options{
		Addr: "review-redis.invalid:6379",
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			redisDials.Add(1)
			return nil, errors.New("redis must not be touched before authentication")
		},
		MaxRetries: -1,
	})
	t.Cleanup(func() { require.NoError(t, fixture.app.Redis.Close()) })
	request := testutil.NewJSONRequest(t, nil)
	request.RequestCtx.Request.SetBody(body)
	request.RequestCtx.Request.Header.Set(
		metareview.TimestampHeader,
		strconv.FormatInt(now.Unix(), 10),
	)
	request.RequestCtx.Request.Header.Set(metareview.NonceHeader, nonce)
	request.RequestCtx.Request.Header.Set(
		metareview.SignatureHeader,
		"sha256="+hex.EncodeToString(make([]byte, sha256.Size)),
	)

	require.NoError(t, fixture.app.ProvisionMetaMessengerReviewCredential(request))
	require.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
	require.Zero(t, redisDials.Load(), "invalid broker auth must not dial Redis")
}

func TestMetaMessengerReviewBrokerRejectsSharedSecretMismatchBeforeRedisOrTenantStorage(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*metareview.ProvisionRequest) error
	}{
		{
			name: "Messenger App Secret",
			mutate: func(request *metareview.ProvisionRequest) error {
				value, err := metareview.MessengerAppSecretKeyID(
					"different-review-handler-meta-app-secret-at-least-32-bytes",
				)
				request.MessengerAppSecretKeyID = value
				return err
			},
		},
		{
			name: "provider-proof secret",
			mutate: func(request *metareview.ProvisionRequest) error {
				value, err := metareview.ProviderProofSecretKeyID(
					"different-review-handler-provider-proof-at-least-32-bytes",
				)
				request.ProviderProofSecretKeyID = value
				return err
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReviewHandlerFixture(t)
			now := time.Now().UTC()
			messengerAppSecretKeyID, err := metareview.MessengerAppSecretKeyID(
				fixture.app.Config.MetaMessengerOnboarding.AppSecret,
			)
			require.NoError(t, err)
			providerProofSecretKeyID, err := metareview.ProviderProofSecretKeyID(
				fixture.app.Config.MetaMessengerReviewRelay.ProviderProofSecret,
			)
			require.NoError(t, err)
			provisionRequest := metareview.ProvisionRequest{
				Version:                  metareview.Version,
				Mode:                     metareview.Mode,
				Tuple:                    fixture.tuple,
				MessengerAppSecretKeyID:  messengerAppSecretKeyID,
				ProviderProofSecretKeyID: providerProofSecretKeyID,
			}
			require.NoError(t, testCase.mutate(&provisionRequest))
			body, err := metareview.EncodeProvisionRequest(provisionRequest, now)
			require.NoError(t, err)
			nonce, err := metareview.NewNonce()
			require.NoError(t, err)
			protocol, err := metareview.NewProtocol(
				fixture.app.Config.MetaMessengerReviewRelay.BrokerAuthSecret,
				fixture.app.Config.MetaMessengerReviewRelay.BrokerWrapSecret,
			)
			require.NoError(t, err)
			signature, err := protocol.SignProvisionRequest(now.Unix(), nonce, body)
			require.NoError(t, err)

			var redisDials atomic.Int32
			fixture.app.Redis = redis.NewClient(&redis.Options{
				Addr: "review-redis.invalid:6379",
				Dialer: func(context.Context, string, string) (net.Conn, error) {
					redisDials.Add(1)
					return nil, errors.New("Redis must not be touched for a shared-secret mismatch")
				},
				MaxRetries: -1,
			})
			t.Cleanup(func() { require.NoError(t, fixture.app.Redis.Close()) })
			request := testutil.NewJSONRequest(t, nil)
			request.RequestCtx.Request.SetBody(body)
			request.RequestCtx.Request.Header.Set(
				metareview.TimestampHeader,
				strconv.FormatInt(now.Unix(), 10),
			)
			request.RequestCtx.Request.Header.Set(metareview.NonceHeader, nonce)
			request.RequestCtx.Request.Header.Set(metareview.SignatureHeader, signature)

			require.NoError(t, fixture.app.ProvisionMetaMessengerReviewCredential(request))
			require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
			require.Zero(t, redisDials.Load(), "shared-secret mismatch must not dial Redis")
		})
	}
}

func TestMetaMessengerReviewAccountTrustRequiresExactReadyInboundOnlyBinding(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	account := fixture.readyAccount(t)
	require.True(t, fixture.app.configuredMetaMessengerReviewAccount(&account))
	require.True(t, fixture.app.readyMetaMessengerReviewAccount(&account))

	connectedAt := time.Now().UTC()
	tests := []struct {
		name                       string
		mutate                     func(*models.ChannelAccount)
		deploymentIdentityStillHit bool
	}{
		{
			name: "different account id",
			mutate: func(account *models.ChannelAccount) {
				account.ID = uuid.New()
			},
		},
		{
			name: "different organization",
			mutate: func(account *models.ChannelAccount) {
				account.OrganizationID = uuid.New()
			},
		},
		{
			name: "different Page",
			mutate: func(account *models.ChannelAccount) {
				account.ExternalAccountID = "1038752885977373"
			},
		},
		{
			name: "noncanonical provider spelling",
			mutate: func(account *models.ChannelAccount) {
				account.Provider = "Relay"
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "missing review marker",
			mutate: func(account *models.ChannelAccount) {
				delete(account.Metadata, "review_relay_mode")
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "wrong generation marker",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["review_generation"] = uuid.NewString()
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "wrong business marker",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_business_id"] = "3852210034910980"
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "wrong app marker",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_app_id"] = "1035383549213573"
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "review readiness false",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["review_ready"] = false
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "subscription unverified",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["subscription_verified"] = false
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "active instead of pending",
			mutate: func(account *models.ChannelAccount) {
				account.Status = models.ChannelAccountStatusActive
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "connected timestamp present",
			mutate: func(account *models.ChannelAccount) {
				account.ConnectedAt = &connectedAt
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "outbound enabled",
			mutate: func(account *models.ChannelAccount) {
				account.Config["outbound_enabled"] = true
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "AI reply enabled",
			mutate: func(account *models.ChannelAccount) {
				account.Config["ai_reply_enabled"] = true
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "wrong relay URL",
			mutate: func(account *models.ChannelAccount) {
				account.Config["relay_url"] = "https://other-relay.example.test/v1/accounts/messenger/" + fixture.tuple.PageID
			},
			deploymentIdentityStillHit: true,
		},
		{
			name: "wrong provider owner business",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_app_owner_business_id"] = "2018290039073162"
			},
			deploymentIdentityStillHit: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := fixture.readyAccount(t)
			testCase.mutate(&candidate)
			assert.Equal(
				t,
				testCase.deploymentIdentityStillHit,
				fixture.app.configuredMetaMessengerReviewAccount(&candidate),
				"the denial identity predicate must not depend on mutable readiness markers",
			)
			assert.False(t, fixture.app.readyMetaMessengerReviewAccount(&candidate))
		})
	}
}

func TestMetaMessengerReviewExpiredGrantStillMatchesDenialIdentity(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	account := fixture.readyAccount(t)
	_, err := fixture.app.metaMessengerOnboardingSettings()
	require.NoError(t, err)
	fixture.app.Config.MetaMessengerReviewRelay.ExpiresAt = time.Now().UTC().
		Add(-time.Minute).
		Format(time.RFC3339Nano)

	_, _, err = fixture.app.metaMessengerReviewSettings(time.Now().UTC())
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)
	_, err = fixture.app.metaMessengerOnboardingSettings()
	require.NoError(t, err, "structural settings remain available for post-expiry deprovisioning")
	request := testutil.NewRequest(t)
	testutil.SetAuthContext(request, fixture.orgID, uuid.New())
	require.NoError(t, fixture.app.StartMetaMessengerOnboarding(request))
	testutil.AssertErrorResponse(
		t,
		request,
		fasthttp.StatusServiceUnavailable,
		"unavailable or expired",
	)
	assert.True(t, fixture.app.configuredMetaMessengerReviewAccount(&account))
	assert.False(t, fixture.app.readyMetaMessengerReviewAccount(&account))
}

func TestMetaMessengerReviewInventoryAllowsOnlyPinnedOwnedMessagingPage(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	businesses := []metaMessengerBusinessSummary{
		{ID: "3852210034910978", Name: "Other Business"},
		{ID: fixture.tuple.MetaBusinessID, Name: "Pinned Business"},
	}
	pages := []metaMessengerStoredPage{
		{metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: fixture.tuple.MetaBusinessID,
			PageID:     "1038752885977371",
			Ownership:  metaMessengerOwnershipOwned,
			Selectable: true,
			Tasks:      []string{"MESSAGING"},
		}},
		{metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: fixture.tuple.MetaBusinessID,
			PageID:     fixture.tuple.PageID,
			Ownership:  metaMessengerOwnershipOwned,
			Selectable: true,
			Tasks:      []string{"MESSAGING"},
		}},
	}

	selectedBusinesses, selectedPages, err := fixture.app.filterMetaMessengerReviewInventory(
		fixture.orgID,
		businesses,
		pages,
	)
	require.NoError(t, err)
	require.Len(t, selectedBusinesses, 1)
	require.Len(t, selectedPages, 1)
	assert.Equal(t, fixture.tuple.MetaBusinessID, selectedBusinesses[0].ID)
	assert.Equal(t, fixture.tuple.PageID, selectedPages[0].PageID)

	wrongOrgBusinesses, wrongOrgPages, err := fixture.app.filterMetaMessengerReviewInventory(
		uuid.New(),
		businesses,
		pages,
	)
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)
	assert.Nil(t, wrongOrgBusinesses)
	assert.Nil(t, wrongOrgPages)

	pages[1].Selectable = false
	_, _, err = fixture.app.filterMetaMessengerReviewInventory(fixture.orgID, businesses, pages)
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)
	assert.NotContains(t, err.Error(), metaMessengerDisabledAssignment)

	pages[1].DisabledReason = metaMessengerDisabledAssignment
	_, _, err = fixture.app.filterMetaMessengerReviewInventory(fixture.orgID, businesses, pages)
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)
	assert.Contains(t, err.Error(), metaMessengerDisabledAssignment)
}

func TestMetaMessengerReviewCredentialBrokerRequiresOneExactCredentialPair(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review credential broker organization",
		Slug:      "review-credential-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	account := fixture.readyAccount(t)
	require.NoError(t, db.Create(&account).Error)
	webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)

	loaded, callbackURL, err := fixture.app.loadMetaMessengerReviewCredential(
		context.Background(),
		fixture.tuple,
		fixture.app.Config.MetaMessengerReviewRelay,
	)
	require.NoError(t, err)
	assert.Equal(t, webhook.ID, loaded.ID)
	assert.Equal(t, webhook.Version, loaded.Version)
	assert.Equal(t, reviewHandlerTestInboundSecret, loaded.InboundSecret)
	assert.Equal(
		t,
		reviewHandlerTestReReplyBaseURL+"/api/webhooks/channels/"+fixture.accountID.String(),
		callbackURL,
	)

	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("id = ?", oauth.ID).
		Update("metadata", models.JSONB{
			"app_id":           "1035383549213573",
			"page_id":          fixture.tuple.PageID,
			"meta_business_id": fixture.tuple.MetaBusinessID,
		}).Error)
	_, _, err = fixture.app.loadMetaMessengerReviewCredential(
		context.Background(),
		fixture.tuple,
		fixture.app.Config.MetaMessengerReviewRelay,
	)
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)

	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("id = ?", oauth.ID).
		Update("metadata", models.JSONB{
			"app_id":           fixture.tuple.MetaAppID,
			"page_id":          fixture.tuple.PageID,
			"meta_business_id": fixture.tuple.MetaBusinessID,
		}).Error)
	createReviewHandlerWebhookCredential(t, db, fixture, 2)
	_, _, err = fixture.app.loadMetaMessengerReviewCredential(
		context.Background(),
		fixture.tuple,
		fixture.app.Config.MetaMessengerReviewRelay,
	)
	require.ErrorIs(t, err, errMetaMessengerReviewUnavailable)
}

func TestEnsureMetaMessengerWebhookCredentialStoresOnlyInboundSecretForReview(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review inbound-only credential organization",
		Slug:      "review-inbound-only-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	reviewAccount := fixture.readyAccount(t)
	require.NoError(t, db.Create(&reviewAccount).Error)

	var reviewCredential models.ChannelCredential
	var reviewCreated bool
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		reviewCredential, reviewCreated, err = fixture.app.ensureMetaMessengerWebhookCredentialTx(
			tx,
			fixture.orgID,
			fixture.accountID,
			true,
		)
		return err
	}))
	require.True(t, reviewCreated)
	encryptedInbound, inboundExists := reviewCredential.CredentialBlob["inbound_secret"].(string)
	require.True(t, inboundExists)
	assert.True(t, appcrypto.IsEncrypted(encryptedInbound))
	_, outboundExists := reviewCredential.CredentialBlob["outbound_secret"]
	assert.False(t, outboundExists, "review credentials must have no outbound capability")
	inboundPlaintext, err := appcrypto.Decrypt(encryptedInbound, reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	assert.NotEmpty(t, inboundPlaintext)

	normalAccount := reviewAccount
	normalAccount.ID = uuid.New()
	normalAccount.ExternalAccountID = strconv.FormatUint(
		reviewHandlerTestPageIDBase+reviewHandlerTestMetaIDSequence.Add(1),
		10,
	)
	normalAccount.Name = "Normal Messenger relay account"
	normalAccount.Metadata = models.JSONB{}
	normalAccount.Config = models.JSONB{
		"outbound_enabled": false,
		"ai_reply_enabled": false,
	}
	require.NoError(t, db.Create(&normalAccount).Error)
	var normalCredential models.ChannelCredential
	var normalCreated bool
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		normalCredential, normalCreated, err = fixture.app.ensureMetaMessengerWebhookCredentialTx(
			tx,
			fixture.orgID,
			normalAccount.ID,
			false,
		)
		return err
	}))
	require.True(t, normalCreated)
	normalEncryptedInbound, inboundExists := normalCredential.CredentialBlob["inbound_secret"].(string)
	require.True(t, inboundExists)
	normalEncryptedOutbound, outboundExists := normalCredential.CredentialBlob["outbound_secret"].(string)
	require.True(t, outboundExists)
	assert.True(t, appcrypto.IsEncrypted(normalEncryptedInbound))
	assert.True(t, appcrypto.IsEncrypted(normalEncryptedOutbound))
	normalInbound, err := appcrypto.Decrypt(normalEncryptedInbound, reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	normalOutbound, err := appcrypto.Decrypt(normalEncryptedOutbound, reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	assert.NotEmpty(t, normalInbound)
	assert.NotEmpty(t, normalOutbound)
	assert.NotEqual(t, normalInbound, normalOutbound)
}

func TestMetaMessengerReviewWebhookPersistenceFenceRejectsRevocationAndGenerationMismatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review persistence fence organization",
		Slug:      "review-persistence-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	account := fixture.readyAccount(t)
	require.NoError(t, db.Create(&account).Error)
	webhook := createReviewHandlerWebhookCredential(t, db, fixture, 1)
	body := []byte(`{"external_account_id":"` + fixture.tuple.PageID + `","events":[]}`)
	headers := reviewHandlerWebhookHeaders(t, fixture, webhook, body)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		locked, err := fixture.app.lockMetaMessengerReviewWebhookPersistenceBinding(
			tx,
			fixture.orgID,
			fixture.accountID,
			headers,
			body,
		)
		if err != nil {
			return err
		}
		require.Equal(t, fixture.accountID, locked.ID)
		return nil
	}))

	wrongGeneration := headers.Clone()
	wrongGeneration.Set(metareview.GenerationHeader, uuid.NewString())
	err := db.Transaction(func(tx *gorm.DB) error {
		_, err := fixture.app.lockMetaMessengerReviewWebhookPersistenceBinding(
			tx,
			fixture.orgID,
			fixture.accountID,
			wrongGeneration,
			body,
		)
		return err
	})
	require.ErrorIs(t, err, errMetaMessengerReviewWebhookInactive)

	now := time.Now().UTC()
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("id = ?", webhook.ID).
		Updates(map[string]any{
			"status":     models.ChannelCredentialStatusRevoked,
			"revoked_at": now,
		}).Error)
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := fixture.app.lockMetaMessengerReviewWebhookPersistenceBinding(
			tx,
			fixture.orgID,
			fixture.accountID,
			headers,
			body,
		)
		return err
	})
	require.ErrorIs(t, err, errMetaMessengerReviewWebhookInactive)
}

func TestMetaMessengerReviewGenericDeleteUsesDeploymentIdentityAfterMarkerCorruption(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review generic delete fence organization",
		Slug:      "review-delete-fence-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	admin := integrationTestUser(
		t,
		fixture.app,
		fixture.orgID,
		models.ResourceChannelAccounts+":"+models.ActionDelete,
	)
	enableBookingCommerceTestEntitlement(
		t,
		db,
		fixture.orgID,
		admin.ID,
		"omnichannel.enabled",
	)
	account := fixture.readyAccount(t)
	delete(account.Metadata, "management_mode")
	delete(account.Metadata, "review_relay_mode")
	account.CreatedByID = &admin.ID
	account.UpdatedByID = &admin.ID
	require.NoError(t, db.Create(&account).Error)
	webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)

	request := testutil.NewRequest(t)
	testutil.SetAuthContext(request, fixture.orgID, admin.ID)
	testutil.SetPathParam(request, "id", fixture.accountID.String())
	require.NoError(t, fixture.app.DeleteChannelAccount(request))
	testutil.AssertErrorResponse(
		t,
		request,
		fasthttp.StatusConflict,
		"can only be changed through review deprovisioning",
	)

	var persistedAccount models.ChannelAccount
	require.NoError(t, db.Unscoped().First(&persistedAccount, "id = ?", fixture.accountID).Error)
	assert.False(t, persistedAccount.DeletedAt.Valid)
	var persistedCredentials []models.ChannelCredential
	require.NoError(t, db.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.orgID,
		fixture.accountID,
	).Order("kind, version").Find(&persistedCredentials).Error)
	require.Len(t, persistedCredentials, 2)
	for _, credential := range persistedCredentials {
		assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
		assert.NotEmpty(t, credential.CredentialBlob)
		switch credential.Kind {
		case models.ChannelCredentialKindWebhook:
			assert.Equal(t, webhook.ID, credential.ID)
			assert.Equal(t, webhook.CredentialBlob, credential.CredentialBlob)
		case models.ChannelCredentialKindOAuth:
			assert.Equal(t, oauth.ID, credential.ID)
			assert.Equal(t, oauth.CredentialBlob, credential.CredentialBlob)
		default:
			require.Fail(t, "unexpected credential kind", credential.Kind)
		}
	}
}

func TestFinalizeMetaMessengerReviewDeprovisionCancelsQueuedWorkAndErasesCredentials(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review deprovision organization",
		Slug:      "review-deprovision-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	user := testutil.CreateTestUser(t, db, fixture.orgID)
	account := fixture.readyAccount(t)
	operationID := uuid.New()
	account.Metadata[metaMessengerSubscriptionDesiredStateKey] = metaMessengerSubscriptionDesiredUnsubscribed
	account.Metadata[metaMessengerSubscriptionOperationIDKey] = operationID.String()
	account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeConfirmed
	account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	require.NoError(t, db.Create(&account).Error)
	webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)
	queued, dispatching := createReviewHandlerOutboxJobs(t, db, account)

	require.NoError(t, fixture.app.finalizeMetaMessengerReviewDeprovision(
		fixture.orgID,
		fixture.accountID,
		user.ID,
		operationID,
	))

	var persistedAccount models.ChannelAccount
	require.NoError(t, db.Unscoped().First(&persistedAccount, "id = ?", account.ID).Error)
	assert.True(t, persistedAccount.DeletedAt.Valid)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, persistedAccount.Status)
	assert.Equal(t, false, persistedAccount.Config["outbound_enabled"])
	assert.Equal(t, false, persistedAccount.Config["ai_reply_enabled"])
	assert.Equal(t, false, persistedAccount.Metadata["review_ready"])
	assert.Nil(t, persistedAccount.ConnectedAt)

	for _, credentialID := range []uuid.UUID{webhook.ID, oauth.ID} {
		var persisted models.ChannelCredential
		require.NoError(t, db.First(&persisted, "id = ?", credentialID).Error)
		assert.Equal(t, models.ChannelCredentialStatusRevoked, persisted.Status)
		assert.Empty(t, persisted.CredentialBlob)
		assert.NotNil(t, persisted.RevokedAt)
	}

	var persistedQueued models.OutboxJob
	require.NoError(t, db.First(&persistedQueued, "id = ?", queued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, persistedQueued.Status)
	assert.Equal(t, reviewHandlerTestDeprovisionedError, persistedQueued.LastErrorCode)
	assert.NotNil(t, persistedQueued.FailedAt)
	assert.Nil(t, persistedQueued.LockedAt)
	assert.Empty(t, persistedQueued.LockedBy)

	var persistedDispatching models.OutboxJob
	require.NoError(t, db.First(&persistedDispatching, "id = ?", dispatching.ID).Error)
	assert.Equal(
		t,
		models.OutboxJobStatusDispatching,
		persistedDispatching.Status,
		"a job that already crossed the dispatch fence is not retroactively cancelled",
	)
}

func TestMetaMessengerReviewDeprovisionRejectsOAuthFromDifferentBusinessBeforeQuarantine(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review deprovision business fence organization",
		Slug:      "review-deprovision-business-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	user := testutil.CreateTestUser(t, db, fixture.orgID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(
		t,
		db,
		fixture.orgID,
		user.ID,
		"omnichannel.enabled",
	)
	account := fixture.readyAccount(t)
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	require.NoError(t, db.Create(&account).Error)
	webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)
	wrongBusinessMetadata := models.JSONB{
		"app_id":           fixture.tuple.MetaAppID,
		"page_id":          fixture.tuple.PageID,
		"meta_business_id": "3852210034910980",
	}
	require.NoError(t, db.Model(&models.ChannelCredential{}).
		Where("id = ?", oauth.ID).
		Update("metadata", wrongBusinessMetadata).Error)

	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodDelete)
	testutil.SetAuthContext(request, fixture.orgID, user.ID)
	testutil.SetPathParam(request, "id", fixture.accountID.String())
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusBadGateway, "quarantined")

	var persistedAccount models.ChannelAccount
	require.NoError(t, db.First(&persistedAccount, "id = ?", account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, persistedAccount.Status)
	assert.Equal(t, false, persistedAccount.Metadata["review_ready"])
	assert.Equal(t, "review_remote_cleanup_pending", persistedAccount.Metadata["onboarding_state"])
	assert.Equal(t, false, persistedAccount.Config["outbound_enabled"])
	assert.Equal(t, false, persistedAccount.Config["ai_reply_enabled"])
	var persistedWebhook models.ChannelCredential
	require.NoError(t, db.First(&persistedWebhook, "id = ?", webhook.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, persistedWebhook.Status)
	assert.Empty(t, persistedWebhook.CredentialBlob)
	assert.NotNil(t, persistedWebhook.RevokedAt)
}

func TestMetaMessengerReviewDeprovisionSurvivesMissingOrLapsedEntitlement(t *testing.T) {
	for _, testCase := range []struct {
		name                 string
		configureEntitlement func(*testing.T, *gorm.DB, uuid.UUID, uuid.UUID)
	}{
		{name: "absent"},
		{
			name: "lapsed",
			configureEntitlement: func(t *testing.T, db *gorm.DB, orgID, userID uuid.UUID) {
				t.Helper()
				enableBookingCommerceTestEntitlement(
					t,
					db,
					orgID,
					userID,
					"omnichannel.enabled",
				)
				require.NoError(t, db.Model(&models.Subscription{}).
					Where("organization_id = ?", orgID).
					Update("status", models.SubscriptionStatusExpired).Error)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			fixture := newReviewHandlerFixture(t)
			fixture.app.DB = db
			organization := &models.Organization{
				BaseModel: models.BaseModel{ID: fixture.orgID},
				Name:      "Review cleanup entitlement organization",
				Slug:      "review-cleanup-entitlement-" + uuid.NewString(),
			}
			require.NoError(t, db.Create(organization).Error)
			admin := integrationTestUser(
				t,
				fixture.app,
				fixture.orgID,
				models.ResourceChannelAccounts+":"+models.ActionDelete,
			)
			if testCase.configureEntitlement != nil {
				testCase.configureEntitlement(t, db, fixture.orgID, admin.ID)
			}

			account := fixture.readyAccount(t)
			account.CreatedByID = &admin.ID
			account.UpdatedByID = &admin.ID
			require.NoError(t, db.Create(&account).Error)
			webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)

			var deleteCalls atomic.Int32
			var getCalls atomic.Int32
			fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method {
				case http.MethodDelete:
					deleteCalls.Add(1)
					_, _ = writer.Write([]byte(`{"success":true}`))
				case http.MethodGet:
					getCalls.Add(1)
					_, _ = writer.Write([]byte(`{"data":[]}`))
				default:
					writer.WriteHeader(http.StatusMethodNotAllowed)
				}
			})

			request := testutil.NewRequest(t)
			request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodDelete)
			testutil.SetAuthContext(request, fixture.orgID, admin.ID)
			testutil.SetPathParam(request, "id", fixture.accountID.String())
			require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(request))
			require.Equal(
				t,
				fasthttp.StatusOK,
				testutil.GetResponseStatusCode(request),
				string(testutil.GetResponseBody(request)),
			)
			assert.EqualValues(t, 1, deleteCalls.Load())
			assert.EqualValues(t, 1, getCalls.Load())

			var tombstone models.ChannelAccount
			require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", fixture.accountID).Error)
			assert.True(t, tombstone.DeletedAt.Valid)
			assert.Equal(t, "review_deprovisioned", stringConfigValue(tombstone.Metadata, "onboarding_state"))
			for _, credentialID := range []uuid.UUID{webhook.ID, oauth.ID} {
				var credential models.ChannelCredential
				require.NoError(t, db.First(&credential, "id = ?", credentialID).Error)
				assert.Equal(t, models.ChannelCredentialStatusRevoked, credential.Status)
				assert.Empty(t, credential.CredentialBlob)
			}
			var deletedAuditCount int64
			require.NoError(t, db.Model(&models.AuditLog{}).
				Where(
					"organization_id = ? AND resource_id = ? AND user_id = ? AND action = ?",
					fixture.orgID,
					fixture.accountID,
					admin.ID,
					models.AuditActionDeleted,
				).
				Count(&deletedAuditCount).Error)
			assert.EqualValues(t, 1, deletedAuditCount)
		})
	}
}

func TestMetaMessengerReviewDeprovisionWithoutDeletePermissionDoesNotMutate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review cleanup permission organization",
		Slug:      "review-cleanup-permission-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	user := integrationTestUser(t, fixture.app, fixture.orgID)
	account := fixture.readyAccount(t)
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	require.NoError(t, db.Create(&account).Error)
	webhook, oauth := createReviewHandlerCredentials(t, db, fixture, 1)

	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	})

	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodDelete)
	testutil.SetAuthContext(request, fixture.orgID, user.ID)
	testutil.SetPathParam(request, "id", fixture.accountID.String())
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(request))
	testutil.AssertErrorResponse(
		t,
		request,
		fasthttp.StatusForbidden,
		"Insufficient permissions",
	)
	assert.Zero(t, graphCalls.Load())

	var persistedAccount models.ChannelAccount
	require.NoError(t, db.First(&persistedAccount, "id = ?", fixture.accountID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, persistedAccount.Status)
	assert.True(t, boolConfigValue(persistedAccount.Metadata, "review_ready"))
	assert.True(t, boolConfigValue(persistedAccount.Metadata, "subscription_verified"))
	assert.False(t, persistedAccount.DeletedAt.Valid)
	for _, expected := range []models.ChannelCredential{webhook, oauth} {
		var credential models.ChannelCredential
		require.NoError(t, db.First(&credential, "id = ?", expected.ID).Error)
		assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
		assert.Equal(t, expected.CredentialBlob, credential.CredentialBlob)
		assert.Nil(t, credential.RevokedAt)
	}
	var auditCount int64
	require.NoError(t, db.Model(&models.AuditLog{}).
		Where("organization_id = ? AND resource_id = ?", fixture.orgID, fixture.accountID).
		Count(&auditCount).Error)
	assert.Zero(t, auditCount)
}

func createReviewHandlerCredentials(
	t *testing.T,
	db *gorm.DB,
	fixture reviewHandlerFixture,
	version int,
) (models.ChannelCredential, models.ChannelCredential) {
	t.Helper()
	webhook := createReviewHandlerWebhookCredential(t, db, fixture, version)
	encryptedPageToken, err := appcrypto.Encrypt(
		"review-handler-page-token",
		reviewHandlerTestEncryptionKey,
	)
	require.NoError(t, err)
	oauth := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   fixture.orgID,
		ChannelAccountID: fixture.accountID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          version,
		CredentialBlob:   models.JSONB{"access_token": encryptedPageToken},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		Metadata: models.JSONB{
			"app_id":           fixture.tuple.MetaAppID,
			"page_id":          fixture.tuple.PageID,
			"meta_business_id": fixture.tuple.MetaBusinessID,
		},
	}
	require.NoError(t, db.Create(&oauth).Error)
	return webhook, oauth
}

func createReviewHandlerWebhookCredential(
	t *testing.T,
	db *gorm.DB,
	fixture reviewHandlerFixture,
	version int,
) models.ChannelCredential {
	t.Helper()
	encryptedInbound, err := appcrypto.Encrypt(
		reviewHandlerTestInboundSecret,
		reviewHandlerTestEncryptionKey,
	)
	require.NoError(t, err)
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   fixture.orgID,
		ChannelAccountID: fixture.accountID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          version,
		CredentialBlob:   models.JSONB{"inbound_secret": encryptedInbound},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		Metadata:         models.JSONB{"management_mode": metaMessengerManagementMode},
	}
	require.NoError(t, db.Create(&credential).Error)
	return credential
}

func reviewHandlerWebhookHeaders(
	t *testing.T,
	fixture reviewHandlerFixture,
	credential models.ChannelCredential,
	body []byte,
) http.Header {
	t.Helper()
	reviewProof, err := metareview.SignInboundProof(
		reviewHandlerTestProviderProof,
		fixture.tuple,
		credential.ID.String(),
		credential.Version,
		body,
	)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, []byte(reviewHandlerTestInboundSecret))
	_, _ = mac.Write(body)
	headers := http.Header{}
	headers.Set(
		channelapi.RelaySignatureHeader,
		"sha256="+hex.EncodeToString(mac.Sum(nil)),
	)
	headers.Set(
		channelapi.RelayMetaProviderProofHeader,
		channelapi.SignMetaProviderInboundProof(reviewHandlerTestProviderProof, body),
	)
	headers.Set(metareview.ReviewProofHeader, reviewProof)
	headers.Set(metareview.GenerationHeader, fixture.generation.String())
	headers.Set(metareview.CredentialIDHeader, credential.ID.String())
	headers.Set(metareview.CredentialVersionHeader, strconv.Itoa(credential.Version))
	return headers
}

func createReviewHandlerOutboxJobs(
	t *testing.T,
	db *gorm.DB,
	account models.ChannelAccount,
) (models.OutboxJob, models.OutboxJob) {
	t.Helper()
	contact := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  account.OrganizationID,
		PhoneNumber:     "rv-" + uuid.NewString(),
		WhatsAppAccount: account.Name,
		Tags:            models.JSONBArray{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&contact).Error)
	conversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         account.OrganizationID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		Channel:                account.Channel,
		ExternalConversationID: "review-conversation-" + uuid.NewString(),
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, db.Create(&conversation).Error)
	now := time.Now().UTC()
	lockedAt := now.Add(-time.Minute)
	queued := models.OutboxJob{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   account.OrganizationID,
		ChannelAccountID: account.ID,
		ConversationID:   conversation.ID,
		IdempotencyKey:   "review-queued-" + uuid.NewString(),
		PayloadDigest:    "review-queued-digest",
		Purpose:          models.ChannelPreferencePurposeService,
		Status:           models.OutboxJobStatusProcessing,
		AvailableAt:      now,
		LockedAt:         &lockedAt,
		LockedBy:         "review-test-worker",
		MaxAttempts:      8,
		ProviderState:    models.JSONB{},
		Payload:          models.JSONB{},
	}
	dispatching := queued
	dispatching.ID = uuid.New()
	dispatching.IdempotencyKey = "review-dispatching-" + uuid.NewString()
	dispatching.Status = models.OutboxJobStatusDispatching
	require.NoError(t, db.Create(&queued).Error)
	require.NoError(t, db.Create(&dispatching).Error)
	return queued, dispatching
}
