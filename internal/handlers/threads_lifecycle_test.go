package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const (
	threadsLifecycleTestSecret = "threads-lifecycle-test-app-secret"
	threadsLifecycleTestUserID = "9876543210987654"
)

func TestVerifyThreadsSignedRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC)
	basePayload := map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": now.Add(-time.Hour).Unix(),
		"user_id":   threadsLifecycleTestUserID,
	}

	t.Run("raw base64url and string user id", func(t *testing.T) {
		signed := signThreadsLifecyclePayload(t, basePayload, threadsLifecycleTestSecret, false)
		verified, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		require.NoError(t, err)
		assert.Equal(t, threadsLifecycleTestUserID, verified.UserID)
		assert.Equal(t, now.Add(-time.Hour).Unix(), verified.IssuedAt)
		assert.Len(t, verified.Hash, sha256.Size*2)
	})

	t.Run("padded base64url and numeric user id", func(t *testing.T) {
		payload := cloneThreadsLifecycleTestPayload(basePayload)
		payload["user_id"] = json.Number(threadsLifecycleTestUserID)
		signed := signThreadsLifecyclePayload(t, payload, threadsLifecycleTestSecret, true)
		verified, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		require.NoError(t, err)
		assert.Equal(t, threadsLifecycleTestUserID, verified.UserID)
	})

	t.Run("provider expiry zero and expired retries remain valid", func(t *testing.T) {
		for _, expires := range []int64{0, now.Add(-24 * time.Hour).Unix()} {
			payload := cloneThreadsLifecycleTestPayload(basePayload)
			payload["expires"] = expires
			signed := signThreadsLifecyclePayload(t, payload, threadsLifecycleTestSecret, false)
			_, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
			require.NoError(t, err)
		}
	})

	t.Run("unknown provider fields are forward compatible", func(t *testing.T) {
		payload := cloneThreadsLifecycleTestPayload(basePayload)
		payload["future_meta_field"] = map[string]any{"version": 2}
		signed := signThreadsLifecyclePayload(t, payload, threadsLifecycleTestSecret, false)
		_, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		require.NoError(t, err)
	})

	t.Run("wrong secret", func(t *testing.T) {
		signed := signThreadsLifecyclePayload(t, basePayload, threadsLifecycleTestSecret, false)
		_, err := verifyThreadsSignedRequest(signed, "different-app-secret", now)
		assert.ErrorIs(t, err, errThreadsLifecycleUnauthorized)
	})

	t.Run("tampered payload", func(t *testing.T) {
		signed := signThreadsLifecyclePayload(t, basePayload, threadsLifecycleTestSecret, false)
		parts := strings.Split(signed, ".")
		parts[1] = parts[1][:len(parts[1])-1] + "A"
		_, err := verifyThreadsSignedRequest(strings.Join(parts, "."), threadsLifecycleTestSecret, now)
		assert.ErrorIs(t, err, errThreadsLifecycleUnauthorized)
	})

	t.Run("wrong algorithm", func(t *testing.T) {
		payload := cloneThreadsLifecycleTestPayload(basePayload)
		payload["algorithm"] = "none"
		signed := signThreadsLifecyclePayload(t, payload, threadsLifecycleTestSecret, false)
		_, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		assert.ErrorIs(t, err, errThreadsLifecycleUnauthorized)
	})

	t.Run("future issued at", func(t *testing.T) {
		payload := cloneThreadsLifecycleTestPayload(basePayload)
		payload["issued_at"] = now.Add(threadsLifecycleClockSkew + time.Second).Unix()
		signed := signThreadsLifecyclePayload(t, payload, threadsLifecycleTestSecret, false)
		_, err := verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		assert.ErrorIs(t, err, errThreadsLifecycleUnauthorized)
	})

	t.Run("trailing json", func(t *testing.T) {
		payload, err := json.Marshal(basePayload)
		require.NoError(t, err)
		payload = append(payload, []byte(`{"extra":true}`)...)
		signed := signThreadsLifecycleBytes(payload, threadsLifecycleTestSecret, false)
		_, err = verifyThreadsSignedRequest(signed, threadsLifecycleTestSecret, now)
		assert.ErrorIs(t, err, errThreadsLifecycleMalformed)
	})
}

func TestThreadsLifecycleHelpers(t *testing.T) {
	t.Parallel()
	orgID := uuid.MustParse("e4c74ccc-6ed8-4599-8997-5b8812ed6c5b")
	confirmationCode := "THDEL0123456789ABCDEF0123456789ABCDEF"
	app := &App{Config: &config.Config{Server: config.ServerConfig{BasePath: "/workspace"}}}

	statusURL, err := app.threadsDeletionStatusURL(
		"https://app.example.test/workspace/api/integrations/threads/callback",
		orgID,
		confirmationCode,
	)
	require.NoError(t, err)
	assert.Equal(
		t,
		"https://app.example.test/workspace/api/integrations/threads/"+
			orgID.String()+"/data-deletion/status/"+confirmationCode,
		statusURL,
	)
	assert.True(t, validThreadsDeletionConfirmationCode(confirmationCode))
	assert.False(t, validThreadsDeletionConfirmationCode("THDEL-0123456789ABCDEF0123456789ABCDEF"))
	assert.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9]+$`), confirmationCode)

	status, reason := threadsDeletionDisplayStatus(
		models.PrivacyRequestStatusDenied,
		"Legal hold <case-7>",
	)
	assert.Equal(t, "Denied", status)
	assert.Equal(t, "Legal hold <case-7>", reason)

	config := models.JSONB{}
	hashes := make([]string, 0, threadsLifecycleEventHashLimit+1)
	for index := 0; index <= threadsLifecycleEventHashLimit; index++ {
		hash := fmt.Sprintf("%064x", index+1)
		hashes = append(hashes, hash)
		config = appendThreadsLifecycleEventHash(config, hash)
	}
	assert.False(t, threadsLifecycleEventSeen(config, hashes[0]))
	assert.True(t, threadsLifecycleEventSeen(config, hashes[len(hashes)-1]))
}

func TestThreadsDeauthorizeDisconnectsOnlySignedAccountAndFencesReplay(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	beforeSnapshot, err := fixture.App.loadThreadsIntegrationSnapshot(
		fixture.Organization.ID,
		true,
	)
	require.NoError(t, err)
	issuedAt := time.Now().UTC().Unix()
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": issuedAt,
		"user_id":   fixture.Target.ExternalAccountID,
	}, fixture.Secret, false)

	request := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeauthorizeThreads(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var success map[string]bool
	testutil.ParseJSONResponse(t, request, &success)
	assert.True(t, success["success"])

	var integration models.ProviderIntegration
	require.NoError(t, fixture.App.DB.First(&integration, "id = ?", fixture.Integration.ID).Error)
	assert.True(t, integration.Enabled, "app configuration must remain reconnectable")
	assert.Nil(t, integration.LastSuccessfulAt)
	assert.Equal(t, "provider_deauthorized", integration.LastErrorCode)
	require.NotNil(t, integration.UpdatedByID)
	assert.Equal(t, fixture.Admin.ID, *integration.UpdatedByID)
	afterSnapshot, err := fixture.App.loadThreadsIntegrationSnapshot(
		fixture.Organization.ID,
		true,
	)
	require.NoError(t, err)
	assert.NotEqual(t, beforeSnapshot.Fingerprint, afterSnapshot.Fingerprint)
	deauthorizeEventHash := threadsLifecycleEventHash(
		"deauthorize",
		fixture.AppID,
		fixture.Target.ExternalAccountID,
		verifiedThreadsHash(t, signed, fixture.Secret),
	)
	assert.True(t, threadsLifecycleEventSeen(integration.Config, deauthorizeEventHash))

	var target models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, target.Status)
	assert.False(t, target.IsDefaultOutgoing)
	assert.Equal(t, false, target.Config["outbound_enabled"])
	assert.NotContains(t, target.Metadata, "username")
	assert.Contains(t, target.Name, "Disconnected Threads account")

	var targetOAuth models.ChannelCredential
	require.NoError(t, fixture.App.DB.First(&targetOAuth, "id = ?", fixture.TargetOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, targetOAuth.Status)
	assert.Empty(t, targetOAuth.CredentialBlob)
	var targetWebhook models.ChannelCredential
	require.NoError(t, fixture.App.DB.First(&targetWebhook, "id = ?", fixture.TargetWebhook.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, targetWebhook.Status)
	assert.NotEmpty(t, targetWebhook.CredentialBlob)

	var other models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&other, "id = ?", fixture.Other.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, other.Status)
	var otherOAuth models.ChannelCredential
	require.NoError(t, fixture.App.DB.First(&otherOAuth, "id = ?", fixture.OtherOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, otherOAuth.Status)
	assert.NotEmpty(t, otherOAuth.CredentialBlob)

	// Simulate a successful reauthorization, then deliver the exact Meta retry.
	reconnectedAt := time.Unix(issuedAt, 0).UTC().Add(2 * time.Second)
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", target.ID).
		Updates(map[string]any{
			"status":        models.ChannelAccountStatusActive,
			"connected_at":  reconnectedAt,
			"name":          "Threads @reconnected",
			"last_error":    "",
			"last_error_at": nil,
		}).Error)
	require.NoError(t, fixture.App.DB.Model(&models.ChannelCredential{}).
		Where("id = ?", targetOAuth.ID).
		Updates(map[string]any{
			"status":          models.ChannelCredentialStatusActive,
			"credential_blob": models.JSONB{"access_token": "new-encrypted-token"},
			"revoked_at":      nil,
		}).Error)
	require.NoError(t, fixture.App.DB.Model(&models.ProviderIntegration{}).
		Where("id = ?", integration.ID).
		Updates(map[string]any{
			"last_error_code":    "",
			"last_error_message": "",
			"last_successful_at": reconnectedAt,
		}).Error)

	retry := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeauthorizeThreads(retry))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	require.NoError(t, fixture.App.DB.First(&targetOAuth, "id = ?", targetOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, targetOAuth.Status)
	assert.Equal(t, "new-encrypted-token", targetOAuth.CredentialBlob["access_token"])

	// Evict the original event marker and retry again. ConnectedAt remains the
	// durable generation fence even when the bounded replay cache no longer
	// contains this callback hash.
	require.NoError(t, fixture.App.DB.First(&integration, "id = ?", integration.ID).Error)
	evictedConfig := cloneJSONB(integration.Config)
	for index := 0; index < threadsLifecycleEventHashLimit; index++ {
		evictedConfig = appendThreadsLifecycleEventHash(
			evictedConfig,
			fmt.Sprintf("%064x", index+100),
		)
	}
	assert.False(t, threadsLifecycleEventSeen(evictedConfig, deauthorizeEventHash))
	require.NoError(t, fixture.App.DB.Model(&models.ProviderIntegration{}).
		Where("id = ?", integration.ID).
		Update("config", evictedConfig).Error)

	evictedRetry := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeauthorizeThreads(evictedRetry))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(evictedRetry))
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	require.NoError(t, fixture.App.DB.First(&targetOAuth, "id = ?", targetOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, targetOAuth.Status)
	assert.Equal(t, "new-encrypted-token", targetOAuth.CredentialBlob["access_token"])
}

func TestThreadsDeauthorizeRemainsAvailableWhenReviewIsPendingAndIntegrationDisabled(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	require.NoError(t, fixture.App.DB.Model(&models.ProviderIntegration{}).
		Where("id = ?", fixture.Integration.ID).
		Updates(map[string]any{
			"enabled": false,
			"config": models.JSONB{
				"app_id":            fixture.AppID,
				"redirect_uri":      "https://app.example.test/api/integrations/threads/callback",
				"app_review_status": "pending",
			},
		}).Error)
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": time.Now().UTC().Unix(),
		"user_id":   fixture.Target.ExternalAccountID,
	}, fixture.Secret, false)
	request := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeauthorizeThreads(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	require.NoError(t, fixture.App.DB.First(&fixture.Target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, fixture.Target.Status)
}

func TestThreadsDeauthorizeIgnoresDelayedFirstDeliveryAfterReconnect(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	issuedAt := time.Now().UTC().Add(-10 * time.Minute).Unix()
	reconnectedAt := time.Unix(issuedAt, 0).UTC().Add(5 * time.Minute)
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.Target.ID).
		Updates(map[string]any{
			"connected_at": reconnectedAt,
			"name":         "Threads @new-generation",
		}).Error)
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": issuedAt,
		"user_id":   fixture.Target.ExternalAccountID,
	}, fixture.Secret, false)

	request := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeauthorizeThreads(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var target models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	assert.Equal(t, "Threads @new-generation", target.Name)
	assert.Contains(t, target.Metadata, "username")
	var oauthCredential models.ChannelCredential
	require.NoError(t, fixture.App.DB.First(&oauthCredential, "id = ?", fixture.TargetOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, oauthCredential.Status)
	assert.NotEmpty(t, oauthCredential.CredentialBlob)
	var integration models.ProviderIntegration
	require.NoError(t, fixture.App.DB.First(&integration, "id = ?", fixture.Integration.ID).Error)
	assert.Empty(t, integration.LastErrorCode)
	assert.NotNil(t, integration.LastSuccessfulAt)
}

func TestThreadsDataDeletionIsIdempotentWithoutLocalAccount(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, false)
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": time.Now().UTC().Add(-time.Hour).Unix(),
		"user_id":   "1122334455667788",
	}, fixture.Secret, false)

	first := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeleteThreadsUserData(first))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	var firstResponse threadsDataDeletionResponse
	testutil.ParseJSONResponse(t, first, &firstResponse)
	assert.True(t, validThreadsDeletionConfirmationCode(firstResponse.ConfirmationCode))
	assert.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9]+$`), firstResponse.ConfirmationCode)
	assert.Equal(
		t,
		"https://app.example.test/api/integrations/threads/"+
			fixture.Organization.ID.String()+"/data-deletion/status/"+
			firstResponse.ConfirmationCode,
		firstResponse.URL,
	)

	retry := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeleteThreadsUserData(retry))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
	var retryResponse threadsDataDeletionResponse
	testutil.ParseJSONResponse(t, retry, &retryResponse)
	assert.Equal(t, firstResponse, retryResponse)

	var requestCount int64
	require.NoError(t, fixture.App.DB.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", fixture.Organization.ID).
		Count(&requestCount).Error)
	assert.EqualValues(t, 1, requestCount)
	var eventCount int64
	require.NoError(t, fixture.App.DB.Model(&models.PrivacyRequestEvent{}).
		Where("organization_id = ?", fixture.Organization.ID).
		Count(&eventCount).Error)
	assert.EqualValues(t, 1, eventCount)

	var integration models.ProviderIntegration
	require.NoError(t, fixture.App.DB.First(&integration, "id = ?", fixture.Integration.ID).Error)
	assert.True(t, integration.Enabled)
	assert.Empty(t, integration.LastErrorCode, "an absent provider user must not degrade the workspace")
	assert.NotNil(t, integration.LastSuccessfulAt)
}

func TestThreadsDataDeletionDelayedAfterReconnectKeepsNewGrant(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	issuedAt := time.Now().UTC().Add(-10 * time.Minute).Unix()
	reconnectedAt := time.Unix(issuedAt, 0).UTC().Add(5 * time.Minute)
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.Target.ID).
		Update("connected_at", reconnectedAt).Error)
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": issuedAt,
		"user_id":   fixture.Target.ExternalAccountID,
	}, fixture.Secret, false)

	request := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeleteThreadsUserData(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var response threadsDataDeletionResponse
	testutil.ParseJSONResponse(t, request, &response)
	assert.True(t, validThreadsDeletionConfirmationCode(response.ConfirmationCode))

	var target models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	var oauthCredential models.ChannelCredential
	require.NoError(t, fixture.App.DB.First(&oauthCredential, "id = ?", fixture.TargetOAuth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, oauthCredential.Status)
	assert.NotEmpty(t, oauthCredential.CredentialBlob)
	var privacyCount int64
	require.NoError(t, fixture.App.DB.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", fixture.Organization.ID).
		Count(&privacyCount).Error)
	assert.EqualValues(t, 1, privacyCount, "the deletion workflow must still be recorded")
	var integration models.ProviderIntegration
	require.NoError(t, fixture.App.DB.First(&integration, "id = ?", fixture.Integration.ID).Error)
	assert.Empty(t, integration.LastErrorCode)
	assert.NotNil(t, integration.LastSuccessfulAt)
}

func TestThreadsDataDeletionRejectsWrongSecretWithoutWrites(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": time.Now().UTC().Unix(),
		"user_id":   fixture.Target.ExternalAccountID,
	}, "wrong-secret", false)

	request := newThreadsLifecyclePOSTRequest(t, fixture.Organization.ID, signed)
	require.NoError(t, fixture.App.DeleteThreadsUserData(request))
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))

	var target models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	var privacyCount int64
	require.NoError(t, fixture.App.DB.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", fixture.Organization.ID).
		Count(&privacyCount).Error)
	assert.Zero(t, privacyCount)
}

func TestThreadsLifecycleWrongOrganizationCannotCrossTenant(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, true)
	foreign := newThreadsLifecycleFixture(t, false)
	fixture.App.Config.Database.RLSEnabled = true
	signed := signThreadsLifecyclePayload(t, map[string]any{
		"algorithm": "HMAC-SHA256",
		"issued_at": time.Now().UTC().Unix(),
		"user_id":   fixture.Target.ExternalAccountID,
	}, fixture.Secret, false)

	request := newThreadsLifecyclePOSTRequest(t, foreign.Organization.ID, signed)
	require.NoError(t, fixture.App.DeleteThreadsUserData(request))
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))

	var target models.ChannelAccount
	require.NoError(t, fixture.App.DB.First(&target, "id = ?", fixture.Target.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, target.Status)
	var privacyCount int64
	require.NoError(t, fixture.App.DB.Model(&models.PrivacyRequest{}).
		Where("organization_id IN ?", []uuid.UUID{
			fixture.Organization.ID,
			foreign.Organization.ID,
		}).Count(&privacyCount).Error)
	assert.Zero(t, privacyCount)
}

func TestThreadsDataDeletionStatusEscapesReasonAndHidesSubject(t *testing.T) {
	fixture := newThreadsLifecycleFixture(t, false)
	now := time.Now().UTC()
	request := models.PrivacyRequest{
		BaseModel:             models.BaseModel{ID: uuid.New()},
		OrganizationID:        fixture.Organization.ID,
		RequestNumber:         "THDEL0123456789ABCDEF0123456789ABCDEF",
		Type:                  models.PrivacyRequestTypeDeletion,
		Status:                models.PrivacyRequestStatusDenied,
		SubjectType:           threadsLifecycleSubjectType,
		SubjectKey:            "SECRET-SUBJECT-ID",
		ReceivedChannel:       threadsLifecycleReceivedChannel,
		RequesterProfile:      models.JSONB{},
		RequestDetails:        models.JSONB{},
		VerificationMethod:    threadsLifecycleVerification,
		VerificationTokenHash: strings.Repeat("a", 64),
		ReceivedAt:            now,
		DueAt:                 now.Add(30 * 24 * time.Hour),
		DecisionReason:        `Legal hold <script>alert("x")</script>`,
	}
	require.NoError(t, fixture.App.DB.Create(&request).Error)

	statusRequest := testutil.NewGETRequest(t)
	testutil.SetPathParam(statusRequest, "target_organization_id", fixture.Organization.ID.String())
	testutil.SetPathParam(statusRequest, "confirmation_code", request.RequestNumber)
	require.NoError(t, fixture.App.ThreadsDataDeletionStatus(statusRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(statusRequest))
	body := string(testutil.GetResponseBody(statusRequest))
	assert.Contains(t, body, "Status: <strong>Denied</strong>")
	assert.Contains(t, body, "&lt;script&gt;")
	assert.NotContains(t, body, "<script>")
	assert.NotContains(t, body, request.SubjectKey)
	assert.Equal(t, "no-store", string(statusRequest.RequestCtx.Response.Header.Peek("Cache-Control")))
	assert.NotEmpty(t, statusRequest.RequestCtx.Response.Header.Peek("Content-Security-Policy"))

	unknown := testutil.NewGETRequest(t)
	testutil.SetPathParam(unknown, "target_organization_id", fixture.Organization.ID.String())
	testutil.SetPathParam(unknown, "confirmation_code", "THDELFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	require.NoError(t, fixture.App.ThreadsDataDeletionStatus(unknown))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(unknown))
}

type threadsLifecycleFixture struct {
	App           *App
	Organization  *models.Organization
	Admin         *models.User
	Integration   models.ProviderIntegration
	Target        models.ChannelAccount
	TargetOAuth   models.ChannelCredential
	TargetWebhook models.ChannelCredential
	Other         models.ChannelAccount
	OtherOAuth    models.ChannelCredential
	AppID         string
	Secret        string
}

func newThreadsLifecycleFixture(t *testing.T, withAccounts bool) threadsLifecycleFixture {
	t.Helper()
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	appID := fmt.Sprintf("19%d", time.Now().UTC().UnixNano())
	secret := threadsLifecycleTestSecret + "-" + appID
	encryptedSecret, err := appcrypto.Encrypt(secret, integrationTestEncryptionKey)
	require.NoError(t, err)
	encryptedVerifyToken, err := appcrypto.Encrypt(
		"threads-lifecycle-test-verify-token",
		integrationTestEncryptionKey,
	)
	require.NoError(t, err)
	now := time.Now().UTC()
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &appID,
		Enabled:        true,
		Config: approvedThreadsTestConfig(t, models.JSONB{
			"app_id":       appID,
			"redirect_uri": "https://app.example.test/api/integrations/threads/callback",
		}, appID),
		CredentialData: models.JSONB{
			"app_secret":           encryptedSecret,
			"webhook_verify_token": encryptedVerifyToken,
		},
		LastSuccessfulAt: &now,
		CreatedByID:      &admin.ID,
		UpdatedByID:      &admin.ID,
	}
	require.NoError(t, app.DB.Create(&integration).Error)
	fixture := threadsLifecycleFixture{
		App:          app,
		Organization: organization,
		Admin:        admin,
		Integration:  integration,
		AppID:        appID,
		Secret:       secret,
	}
	if !withAccounts {
		return fixture
	}

	fixture.Target = createThreadsLifecycleAccount(
		t,
		app.DB,
		organization.ID,
		admin.ID,
		appID,
		appID+"1",
		"Threads @target-secret-username",
	)
	fixture.TargetOAuth = createThreadsLifecycleCredential(
		t,
		app.DB,
		organization.ID,
		fixture.Target.ID,
		models.ChannelCredentialKindOAuth,
		models.JSONB{"access_token": "encrypted-target-token"},
	)
	fixture.TargetWebhook = createThreadsLifecycleCredential(
		t,
		app.DB,
		organization.ID,
		fixture.Target.ID,
		models.ChannelCredentialKindWebhook,
		models.JSONB{"verify_token": "encrypted-webhook-token"},
	)
	fixture.Other = createThreadsLifecycleAccount(
		t,
		app.DB,
		organization.ID,
		admin.ID,
		appID,
		appID+"2",
		"Threads @other-account",
	)
	fixture.OtherOAuth = createThreadsLifecycleCredential(
		t,
		app.DB,
		organization.ID,
		fixture.Other.ID,
		models.ChannelCredentialKindOAuth,
		models.JSONB{"access_token": "encrypted-other-token"},
	)
	return fixture
}

func createThreadsLifecycleAccount(
	t *testing.T,
	db *gorm.DB,
	orgID, adminID uuid.UUID,
	appID, externalID, name string,
) models.ChannelAccount {
	t.Helper()
	connectedAt := time.Now().UTC().Add(-24 * time.Hour)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              name,
		ExternalAccountID: externalID,
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{"text": true},
		Config:            models.JSONB{"outbound_enabled": true, "private_setting": "remove"},
		Metadata: models.JSONB{
			"app_id":              appID,
			"username":            "private-username",
			"profile_picture_url": "https://private.example/avatar.jpg",
		},
		IsDefaultOutgoing: true,
		ConnectedAt:       &connectedAt,
		CreatedByID:       &adminID,
		UpdatedByID:       &adminID,
	}
	require.NoError(t, db.Create(&account).Error)
	return account
}

func createThreadsLifecycleCredential(
	t *testing.T,
	db *gorm.DB,
	orgID, accountID uuid.UUID,
	kind models.ChannelCredentialKind,
	blob models.JSONB,
) models.ChannelCredential {
	t.Helper()
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ChannelAccountID: accountID,
		Kind:             kind,
		Version:          1,
		CredentialBlob:   blob,
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "test:v1",
		Metadata:         models.JSONB{"private": "credential-metadata"},
	}
	require.NoError(t, db.Create(&credential).Error)
	return credential
}

func newThreadsLifecyclePOSTRequest(
	t *testing.T,
	orgID uuid.UUID,
	signedRequest string,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
	request.RequestCtx.Request.SetBodyString(url.Values{
		"signed_request": {signedRequest},
	}.Encode())
	testutil.SetPathParam(request, "target_organization_id", orgID.String())
	return request
}

func signThreadsLifecyclePayload(
	t *testing.T,
	payload any,
	secret string,
	padded bool,
) string {
	t.Helper()
	payloadJSON, err := json.Marshal(payload)
	require.NoError(t, err)
	return signThreadsLifecycleBytes(payloadJSON, secret, padded)
}

func signThreadsLifecycleBytes(payload []byte, secret string, padded bool) string {
	encoding := base64.RawURLEncoding
	if padded {
		encoding = base64.URLEncoding
	}
	payloadSegment := encoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payloadSegment))
	signatureSegment := encoding.EncodeToString(mac.Sum(nil))
	return signatureSegment + "." + payloadSegment
}

func cloneThreadsLifecycleTestPayload(payload map[string]any) map[string]any {
	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
}

func verifiedThreadsHash(t *testing.T, signedRequest, secret string) string {
	t.Helper()
	verified, err := verifyThreadsSignedRequest(
		signedRequest,
		secret,
		time.Now().UTC().Add(time.Minute),
	)
	require.NoError(t, err)
	return verified.Hash
}
