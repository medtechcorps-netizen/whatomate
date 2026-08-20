package threadsplatform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	runtimeClinicID     = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	runtimeComplianceID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

func runtimeManagedConfig() config.ThreadsManagedConfig {
	return config.ThreadsManagedConfig{
		Enabled:                  true,
		AllowedOrganizationIDs:   runtimeClinicID.String(),
		ComplianceOrganizationID: runtimeComplianceID.String(),
		PlatformApps: []config.ThreadsPlatformAppConfig{
			{
				PlatformAppKey:          "primary",
				AppID:                   "123456789012345",
				AppSecret:               "synthetic-runtime-app-secret-000001",
				WebhookVerifyToken:      "synthetic-runtime-webhook-token-001",
				AppReviewStatus:         "approved",
				AppReviewEvidenceSHA256: strings.Repeat("b", 64),
				AppReviewApprovedAt:     "2026-01-02T03:04:05Z",
				ConfigurationGeneration: 7,
			},
		},
	}
}

func TestRuntimeActivationFailsClosedOnEveryRequiredFact(t *testing.T) {
	runtime, err := NewRuntime(runtimeManagedConfig(), config.ThreadsAppReviewConfig{}, "staging")
	require.NoError(t, err)
	valid := ActivationFacts{
		OrganizationID:                runtimeClinicID,
		PlatformAppKey:                "primary",
		HasIntegrationWritePermission: true,
		HasThreadsEntitlement:         true,
	}
	require.NoError(t, runtime.ValidateActivation(valid))

	for _, testCase := range []struct {
		name   string
		mutate func(*ActivationFacts)
		target error
	}{
		{"permission", func(value *ActivationFacts) { value.HasIntegrationWritePermission = false }, ErrPermissionRequired},
		{"entitlement", func(value *ActivationFacts) { value.HasThreadsEntitlement = false }, ErrEntitlementRequired},
		{"allowlist", func(value *ActivationFacts) { value.OrganizationID = uuid.New() }, ErrOrganizationNotAllowed},
		{"compliance tenant", func(value *ActivationFacts) { value.OrganizationID = runtimeComplianceID }, ErrComplianceOrganization},
		{"app key", func(value *ActivationFacts) { value.PlatformAppKey = "missing" }, ErrUnknownPlatformApp},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			facts := valid
			testCase.mutate(&facts)
			assert.ErrorIs(t, runtime.ValidateActivation(facts), testCase.target)
		})
	}

	disabled, err := NewRuntime(config.ThreadsManagedConfig{}, config.ThreadsAppReviewConfig{}, "production")
	require.NoError(t, err)
	assert.ErrorIs(t, disabled.ValidateActivation(valid), ErrManagedDisabled)
}

func TestRuntimeSupportsAppKeysWithoutExposingCredentialMaterial(t *testing.T) {
	managed := runtimeManagedConfig()
	secondary := managed.PlatformApps[0]
	secondary.PlatformAppKey = "secondary_my"
	secondary.AppID = "987654321098765"
	secondary.AppSecret = "synthetic-secondary-app-secret-00001"
	secondary.WebhookVerifyToken = "synthetic-secondary-webhook-token-01"
	secondary.AppReviewEvidenceSHA256 = strings.Repeat("c", 64)
	managed.PlatformApps = append(managed.PlatformApps, secondary)
	runtime, err := NewRuntime(managed, config.ThreadsAppReviewConfig{}, "staging")
	require.NoError(t, err)

	descriptor, err := runtime.App("secondary_my")
	require.NoError(t, err)
	assert.Equal(t, secondary.AppID, descriptor.AppID)
	assert.Equal(t, uint64(7), descriptor.ConfigurationGeneration)
	encodedDescriptor, err := json.Marshal(descriptor)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedDescriptor), secondary.AppSecret)
	assert.NotContains(t, string(encodedDescriptor), secondary.WebhookVerifyToken)

}

func TestNewRuntimeRevalidatesFullDevelopmentReviewGate(t *testing.T) {
	managed := runtimeManagedConfig()
	productionGate := config.ThreadsAppReviewConfig{
		DevelopmentTestingEnabled: true,
		DevelopmentOrganizationID: runtimeClinicID.String(),
		DevelopmentAppID:          managed.PlatformApps[0].AppID,
		DevelopmentProfileID:      "987654321098765",
	}
	_, err := NewRuntime(managed, productionGate, "production")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden in production")

	invalidNonproductionGate := productionGate
	invalidNonproductionGate.DevelopmentProfileID = ""
	_, err = NewRuntime(managed, invalidNonproductionGate, "staging")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app_id and profile_id must be canonical numeric Meta IDs")
}

func TestRuntimeAllowAllStillExcludesComplianceTenant(t *testing.T) {
	managed := runtimeManagedConfig()
	managed.AllowedOrganizationIDs = ""
	managed.AllowAllOrganizations = true
	runtime, err := NewRuntime(managed, config.ThreadsAppReviewConfig{}, "production")
	require.NoError(t, err)
	assert.True(t, runtime.OrganizationAllowed(uuid.New()))
	assert.False(t, runtime.OrganizationAllowed(runtimeComplianceID))
}

func TestRuntimeSupportsMultipleExplicitOrganizations(t *testing.T) {
	secondClinic := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	managed := runtimeManagedConfig()
	managed.AllowedOrganizationIDs += "," + secondClinic.String()
	runtime, err := NewRuntime(managed, config.ThreadsAppReviewConfig{}, "staging")
	require.NoError(t, err)
	assert.True(t, runtime.OrganizationAllowed(runtimeClinicID))
	assert.True(t, runtime.OrganizationAllowed(secondClinic))
	assert.False(t, runtime.OrganizationAllowed(uuid.New()))
}

func TestEffectiveManagementModePreservesBYOAndRejectsTenantSecretsForManagedMode(t *testing.T) {
	legacy := &models.ProviderIntegration{Provider: "threads"}
	assert.Equal(t, models.ThreadsManagementModeWorkspaceBYO, EffectiveManagementMode(legacy))
	require.NoError(t, ValidateIntegrationManagement(legacy))
	legacy.ManagementMode = " workspace_byo "
	assert.ErrorIs(t, ValidateIntegrationManagement(legacy), ErrInvalidManagementMode)
	legacy.ManagementMode = ""

	appKey := "primary"
	managed := &models.ProviderIntegration{
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &appKey,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, ValidateIntegrationManagement(managed))

	managed.CredentialData["app_secret"] = "encrypted-but-tenant-owned"
	assert.ErrorIs(t, ValidateIntegrationManagement(managed), ErrInvalidManagementMode)
	delete(managed.CredentialData, "app_secret")
	managed.Config["app_id"] = "123456789012345"
	assert.ErrorIs(t, ValidateIntegrationManagement(managed), ErrInvalidManagementMode)
	delete(managed.Config, "app_id")
	managed.ThreadsAppID = new(string)
	assert.ErrorIs(t, ValidateIntegrationManagement(managed), ErrInvalidManagementMode)

	nonThreads := &models.ProviderIntegration{
		Provider:       "qwen",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &appKey,
	}
	assert.ErrorIs(t, ValidateIntegrationManagement(nonThreads), ErrInvalidManagementMode)
}
