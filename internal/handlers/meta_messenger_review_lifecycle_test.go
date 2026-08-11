package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestMetaMessengerReviewDeprovisionFencesBlockedSubscribeAndCompensates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review lifecycle fence organization",
		Slug:      "review-lifecycle-fence-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	user := testutil.CreateTestUser(t, db, fixture.orgID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, fixture.orgID, user.ID, "omnichannel.enabled")

	account := fixture.readyAccount(t)
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	account.Metadata["review_ready"] = false
	account.Metadata["subscription_verified"] = false
	account.Metadata["onboarding_state"] = metaMessengerVerifyingSubscription
	require.NoError(t, db.Create(&account).Error)
	_, oauth := createReviewHandlerCredentials(t, db, fixture, 1)
	operation := newMetaMessengerSubscriptionOperation(oauth, time.Now().UTC())
	account.Metadata = cloneJSONB(account.Metadata)
	writeMetaMessengerSubscriptionOperation(account.Metadata, operation)
	account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ?", account.ID).
		Update("metadata", account.Metadata).Error)

	var stateMu sync.Mutex
	subscribed := false
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	var postStartedOnce sync.Once
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodPost:
			postStartedOnce.Do(func() { close(postStarted) })
			<-releasePost
			stateMu.Lock()
			subscribed = true
			stateMu.Unlock()
			_, _ = writer.Write([]byte(`{"success":true}`))
		case http.MethodDelete:
			deleteCalls.Add(1)
			stateMu.Lock()
			subscribed = false
			stateMu.Unlock()
			_, _ = writer.Write([]byte(`{"success":true}`))
		case http.MethodGet:
			stateMu.Lock()
			present := subscribed
			stateMu.Unlock()
			if present {
				_, _ = writer.Write([]byte(`{"data":[{"id":"` + reviewHandlerTestAppID + `","subscribed_fields":["messages"]}]}`))
			} else {
				_, _ = writer.Write([]byte(`{"data":[]}`))
			}
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": "unexpected method"})
		}
	}))
	defer server.Close()
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		metaMessengerProductionGraphOrigin: server,
	})

	type subscribeResult struct {
		finalizeErr     error
		compensationErr error
	}
	resultCh := make(chan subscribeResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), metaMessengerProviderOperationLimit)
		defer cancel()
		if err := fixture.app.subscribeMetaMessengerPage(ctx, fixture.tuple.PageID, "review-handler-page-token"); err != nil {
			resultCh <- subscribeResult{finalizeErr: err}
			return
		}
		_, finalizeErr := fixture.app.finalizeMetaMessengerPendingAccountOperation(
			fixture.orgID,
			user.ID,
			fixture.accountID,
			operation,
			metaMessengerAwaitingRegistryState,
			true,
			"",
			metaMessengerSubscriptionRemoteSubscribed,
		)
		var compensationErr error
		if finalizeErr != nil {
			compensationErr = fixture.app.compensateMetaMessengerSubscribe(
				ctx,
				fixture.orgID,
				fixture.accountID,
				operation,
				fixture.tuple.PageID,
				"review-handler-page-token",
			)
		}
		resultCh <- subscribeResult{finalizeErr: finalizeErr, compensationErr: compensationErr}
	}()

	select {
	case <-postStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe request did not reach the blocking Graph server")
	}

	firstDelete := newReviewLifecycleDeleteRequest(t, fixture, user.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(firstDelete))
	testutil.AssertErrorResponse(t, firstDelete, fasthttp.StatusServiceUnavailable, "earlier onboarding request drains")

	var quarantined models.ChannelAccount
	require.NoError(t, db.First(&quarantined, "id = ?", fixture.accountID).Error)
	assert.Equal(t, metaMessengerSubscriptionDesiredUnsubscribed, stringConfigValue(quarantined.Metadata, metaMessengerSubscriptionDesiredStateKey))
	assert.Equal(t, operation.ID.String(), stringConfigValue(quarantined.Metadata, metaMessengerSubscriptionFencedOperationIDKey))
	assert.Equal(t, false, quarantined.Metadata[metaMessengerSubscriptionFencedAckKey])
	var retainedOAuth models.ChannelCredential
	require.NoError(t, db.First(&retainedOAuth, "id = ?", oauth.ID).Error)
	assert.NotEmpty(t, retainedOAuth.CredentialBlob, "cleanup token must remain while the old subscribe is in flight")

	close(releasePost)
	result := <-resultCh
	require.ErrorIs(t, result.finalizeErr, errMetaMessengerSubscriptionFence)
	require.NoError(t, result.compensationErr)

	require.NoError(t, db.First(&quarantined, "id = ?", fixture.accountID).Error)
	assert.Equal(t, true, quarantined.Metadata[metaMessengerSubscriptionFencedAckKey])
	assert.Equal(t, metaMessengerSubscriptionRemoteUnsubscribed, stringConfigValue(quarantined.Metadata, metaMessengerSubscriptionRemoteStateKey))

	secondDelete := newReviewLifecycleDeleteRequest(t, fixture, user.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(secondDelete))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondDelete))
	stateMu.Lock()
	assert.False(t, subscribed, "Meta must be unsubscribed after the interleaving completes")
	stateMu.Unlock()
	assert.GreaterOrEqual(t, deleteCalls.Load(), int32(3), "initial cleanup, stale-subscribe compensation, and retry cleanup must all be idempotent")

	var tombstone models.ChannelAccount
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", fixture.accountID).Error)
	assert.True(t, tombstone.DeletedAt.Valid)
	assert.Equal(t, "review_deprovisioned", stringConfigValue(tombstone.Metadata, "onboarding_state"))
	assert.Equal(t, metaMessengerSubscriptionRemoteUnsubscribed, stringConfigValue(tombstone.Metadata, metaMessengerSubscriptionRemoteStateKey))
	require.NoError(t, db.First(&retainedOAuth, "id = ?", oauth.ID).Error)
	assert.Empty(t, retainedOAuth.CredentialBlob, "OAuth token is erased only after the fence is safe")

	duplicateDelete := newReviewLifecycleDeleteRequest(t, fixture, user.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(duplicateDelete))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(duplicateDelete))
}

func TestUnsubscribeMetaMessengerPageConfirmsAbsenceAfterAmbiguousDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusGatewayTimeout)
			_, _ = writer.Write([]byte(`{"error":{"message":"ambiguous timeout"}}`))
			return
		}
		require.Equal(t, http.MethodGet, request.Method)
		_, _ = writer.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)

	require.NoError(t, app.unsubscribeMetaMessengerPage(
		context.Background(),
		metaMessengerTestPageID,
		"page-token",
	))
}

func TestReviewConversionScrubsAllSupersededCredentialBlobs(t *testing.T) {
	db := testutil.SetupTestDB(t)
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review conversion scrub organization",
		Slug:      "review-conversion-scrub-" + uuid.NewString(),
	}
	require.NoError(t, db.Create(organization).Error)
	user := testutil.CreateTestUser(t, db, fixture.orgID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, fixture.orgID, user.ID, "omnichannel.enabled")

	account := fixture.readyAccount(t)
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	delete(account.Metadata, "review_relay_mode")
	delete(account.Metadata, "review_generation")
	account.Metadata["review_ready"] = false
	account.Metadata["subscription_verified"] = false
	account.Metadata["registry_recognized"] = false
	account.Metadata["onboarding_state"] = metaMessengerVerifyingSubscription
	account.Metadata["ownership_evidence_version"] = "owned_pages_v1"
	account.Metadata["ownership_verified_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	account.Config["onboarding_state"] = metaMessengerVerifyingSubscription
	require.NoError(t, db.Create(&account).Error)

	encryptedInbound, err := appcrypto.Encrypt("old-inbound-secret", reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	encryptedOutbound, err := appcrypto.Encrypt("old-outbound-secret", reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	encryptedOldToken, err := appcrypto.Encrypt("old-page-token", reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	now := time.Now().UTC()
	historical := []models.ChannelCredential{
		{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.orgID,
			ChannelAccountID: fixture.accountID,
			Kind:             models.ChannelCredentialKindWebhook,
			Version:          1,
			CredentialBlob: models.JSONB{
				"inbound_secret":  encryptedInbound,
				"outbound_secret": encryptedOutbound,
			},
			Status:     models.ChannelCredentialStatusActive,
			KeyVersion: "app:v1",
			Metadata:   models.JSONB{"management_mode": metaMessengerManagementMode},
		},
		{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.orgID,
			ChannelAccountID: fixture.accountID,
			Kind:             models.ChannelCredentialKindWebhook,
			Version:          2,
			CredentialBlob: models.JSONB{
				"inbound_secret":  encryptedInbound,
				"outbound_secret": encryptedOutbound,
			},
			Status:     models.ChannelCredentialStatusRevoked,
			KeyVersion: "app:v1",
			RevokedAt:  &now,
			Metadata:   models.JSONB{"management_mode": metaMessengerManagementMode},
		},
		{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   fixture.orgID,
			ChannelAccountID: fixture.accountID,
			Kind:             models.ChannelCredentialKindOAuth,
			Version:          1,
			CredentialBlob:   models.JSONB{"access_token": encryptedOldToken},
			Status:           models.ChannelCredentialStatusActive,
			KeyVersion:       "app:v1",
			Metadata: models.JSONB{
				"app_id":           fixture.tuple.MetaAppID,
				"page_id":          fixture.tuple.PageID,
				"meta_business_id": fixture.tuple.MetaBusinessID,
			},
		},
	}
	for index := range historical {
		require.NoError(t, db.Create(&historical[index]).Error)
	}

	encryptedNewToken, err := appcrypto.Encrypt("new-page-token", reviewHandlerTestEncryptionKey)
	require.NoError(t, err)
	page := metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID:   fixture.tuple.MetaBusinessID,
			BusinessName: "Klinik Insan",
			PageID:       fixture.tuple.PageID,
			PageName:     "Klinik Insan Avenue Ampang",
			Ownership:    metaMessengerOwnershipOwned,
			Selectable:   true,
			Tasks:        []string{"MESSAGING"},
		},
		EncryptedPageToken:  encryptedNewToken,
		OwnershipVerifiedAt: now,
	}
	authorization := metaMessengerTokenInspection{
		AppID:     fixture.tuple.MetaAppID,
		Type:      metaMessengerTokenKindUser,
		UserID:    metaMessengerTestUserID,
		Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
		CheckedAt: now,
	}
	staged, err := fixture.app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, fixture.orgID, user.ID),
		fixture.orgID,
		user.ID,
		page,
		metaMessengerTokenInspection{CheckedAt: now},
		authorization,
		"",
	)
	require.NoError(t, err)
	require.Equal(t, fixture.accountID, staged.ID)

	var persisted []models.ChannelCredential
	require.NoError(t, db.Where("channel_account_id = ?", fixture.accountID).
		Order("kind, version").Find(&persisted).Error)
	var currentWebhook, currentOAuth int
	for index := range persisted {
		credential := persisted[index]
		if credential.Status == models.ChannelCredentialStatusActive {
			switch credential.Kind {
			case models.ChannelCredentialKindWebhook:
				currentWebhook++
				_, hasInbound := credential.CredentialBlob["inbound_secret"]
				_, hasOutbound := credential.CredentialBlob["outbound_secret"]
				assert.True(t, hasInbound)
				assert.False(t, hasOutbound, "current review webhook must remain inbound-only")
			case models.ChannelCredentialKindOAuth:
				currentOAuth++
				assert.NotEmpty(t, credential.CredentialBlob["access_token"])
			}
			continue
		}
		assert.Empty(t, credential.CredentialBlob, "every superseded review credential blob must be scrubbed")
	}
	assert.Equal(t, 1, currentWebhook)
	assert.Equal(t, 1, currentOAuth)
}

func newReviewLifecycleDeleteRequest(
	t *testing.T,
	fixture reviewHandlerFixture,
	userID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodDelete)
	testutil.SetAuthContext(request, fixture.orgID, userID)
	testutil.SetPathParam(request, "id", fixture.accountID.String())
	return request
}
