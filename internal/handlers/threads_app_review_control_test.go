package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestThreadsApprovalCannotBeSelfAttestedByIntegrationWriter(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	request := testutil.NewJSONRequest(t, map[string]any{
		"enabled": false,
		"config": map[string]any{
			"app_id":            "123456789012345",
			"redirect_uri":      "https://app.example.test/api/integrations/threads/callback",
			"app_review_status": "approved",
		},
		"credentials": map[string]any{
			"app_secret":           "synthetic-threads-app-secret",
			"webhook_verify_token": "synthetic-threads-webhook-token",
		},
	})
	testutil.SetAuthContext(request, organization.ID, admin.ID)
	testutil.SetPathParam(request, "provider", integrationProviderThreads)
	require.NoError(t, app.UpdateIntegration(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "platform owner")

	var count int64
	require.NoError(t, app.DB.Model(&models.ProviderIntegration{}).
		Where("organization_id = ? AND provider = ?", organization.ID, integrationProviderThreads).
		Count(&count).Error)
	assert.Zero(t, count, "the forbidden self-attestation must roll back all configuration and credentials")

	controlRequest := testutil.NewJSONRequest(t, map[string]any{"evidence": "synthetic evidence"})
	testutil.SetFullAuthContext(
		controlRequest,
		organization.ID,
		admin.ID,
		admin.RoleID,
		false,
	)
	testutil.SetPathParam(controlRequest, "target_organization_id", organization.ID.String())
	require.NoError(t, app.ApproveOrganizationThreadsAppReview(controlRequest))
	testutil.AssertErrorResponse(t, controlRequest, fasthttp.StatusForbidden, "Platform owner")
}

func TestPlatformOwnerApprovalRecordsBoundEvidenceWithoutActivating(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(t, app.DB, organization.ID, testutil.WithSuperAdmin())
	appSecret, err := appcrypto.Encrypt("synthetic-threads-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	verifyToken, err := appcrypto.Encrypt("synthetic-threads-webhook-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	const appID = "123456789012346"
	binding := appID
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &binding,
		Enabled:        true,
		Config: models.JSONB{
			"app_id":            appID,
			"redirect_uri":      "https://app.example.test/api/integrations/threads/callback",
			"app_review_status": "pending",
		},
		CredentialData: models.JSONB{
			"app_secret":           appSecret,
			"webhook_verify_token": verifyToken,
		},
	}
	require.NoError(t, app.DB.Create(&integration).Error)

	const evidence = "synthetic Meta dashboard approval reference REVIEW-123"
	request := testutil.NewJSONRequest(t, map[string]any{"evidence": evidence})
	testutil.SetFullAuthContext(request, organization.ID, owner.ID, owner.RoleID, true)
	testutil.SetPathParam(request, "target_organization_id", organization.ID.String())
	require.NoError(t, app.ApproveOrganizationThreadsAppReview(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.NotContains(t, string(testutil.GetResponseBody(request)), evidence)

	require.NoError(t, app.DB.First(&integration, "id = ?", integration.ID).Error)
	assert.False(t, integration.Enabled)
	assert.True(t, threadsreview.ProductionApproved(integration.Config))
	assert.NotEqual(t, evidence, stringJSONValue(integration.Config, threadsreview.ApprovalEvidenceKey))
}

func TestThreadsConfigOnlyReviewDowngradeAtomicallyDisconnectsLegacyState(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	const appID = "123456789012347"
	binding := appID
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &binding,
		Enabled:        true,
		Config: approvedThreadsTestConfig(t, models.JSONB{
			"app_id":       appID,
			"redirect_uri": "https://app.example.test/api/integrations/threads/callback",
		}, appID),
		CredentialData: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&integration).Error)
	account := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organization.ID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              "Synthetic Threads account",
		ExternalAccountID: appID + "1",
		Status:            models.ChannelAccountStatusActive,
		Config:            models.JSONB{"outbound_enabled": true},
		Metadata:          models.JSONB{"app_id": appID},
	}
	require.NoError(t, app.DB.Create(&account).Error)
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		ChannelAccountID: account.ID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": "synthetic-encrypted-token"},
		Status:           models.ChannelCredentialStatusActive,
		ExpiresAt:        &expiresAt,
		Metadata:         models.JSONB{"app_id": appID},
	}
	require.NoError(t, app.DB.Create(&credential).Error)

	request := testutil.NewJSONRequest(t, map[string]any{
		"config": map[string]any{"app_review_status": "pending"},
	})
	testutil.SetAuthContext(request, organization.ID, admin.ID)
	testutil.SetPathParam(request, "provider", integrationProviderThreads)
	require.NoError(t, app.UpdateIntegration(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	require.NoError(t, app.DB.First(&integration, "id = ?", integration.ID).Error)
	assert.False(t, integration.Enabled)
	assert.Equal(t, "pending", stringJSONValue(integration.Config, threadsreview.StatusKey))
	_, evidencePresent := integration.Config[threadsreview.ApprovalEvidenceKey]
	assert.False(t, evidencePresent)
	require.NoError(t, app.DB.First(&account, "id = ?", account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
	assert.Equal(t, false, account.Config["outbound_enabled"])
	require.NoError(t, app.DB.First(&credential, "id = ?", credential.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, credential.Status)
	assert.False(t, strings.Contains(string(testutil.GetResponseBody(request)), threadsreview.ApprovalEvidenceKey))
}
