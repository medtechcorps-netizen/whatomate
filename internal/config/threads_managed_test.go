package config_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	managedClinicOrganizationID     = "11111111-1111-4111-8111-111111111111"
	managedComplianceOrganizationID = "22222222-2222-4222-8222-222222222222"
	managedAppSecret                = "synthetic-managed-app-secret-00000001"
	managedWebhookVerifyToken       = "synthetic-webhook-verify-token-000001"
)

func approvedManagedThreadsConfig() config.ThreadsManagedConfig {
	return config.ThreadsManagedConfig{
		Enabled:                  true,
		ReReplyBaseURL:           "https://threads.test.example",
		AllowedOrganizationIDs:   managedClinicOrganizationID,
		ComplianceOrganizationID: managedComplianceOrganizationID,
		PlatformApps: []config.ThreadsPlatformAppConfig{
			{
				PlatformAppKey:          "primary",
				AppID:                   "123456789012345",
				AppSecret:               managedAppSecret,
				WebhookVerifyToken:      managedWebhookVerifyToken,
				AppReviewStatus:         "approved",
				AppReviewEvidenceSHA256: strings.Repeat("a", 64),
				AppReviewApprovedAt:     "2026-01-02T03:04:05Z",
				ConfigurationGeneration: 1,
			},
		},
	}
}

func TestLoad_ManagedThreadsDefaultsOffAndParsesOnePlatformApp(t *testing.T) {
	defaultConfig, err := config.Load(writeConfig(t, ""))
	require.NoError(t, err)
	assert.False(t, defaultConfig.ThreadsManaged.Enabled)
	assert.False(t, defaultConfig.ThreadsManaged.AllowAllOrganizations)
	assert.Empty(t, defaultConfig.ThreadsManaged.PlatformApps)

	content := fmt.Sprintf(`
[app]
environment = "staging"

[threads_managed]
enabled = true
allowed_organization_ids = %q
allow_all_organizations = false
compliance_organization_id = %q
rereply_base_url = "https://threads.test.example"

[[threads_managed.platform_apps]]
platform_app_key = "primary"
app_id = "123456789012345"
app_secret = %q
webhook_verify_token = %q
app_review_status = "approved"
app_review_evidence_sha256 = %q
app_review_approved_at = "2026-01-02T03:04:05Z"
configuration_generation = 1
`,
		managedClinicOrganizationID,
		managedComplianceOrganizationID,
		managedAppSecret,
		managedWebhookVerifyToken,
		strings.Repeat("a", 64),
	)
	loaded, err := config.Load(writeConfig(t, content))
	require.NoError(t, err)
	require.Len(t, loaded.ThreadsManaged.PlatformApps, 1)
	assert.Equal(t, "primary", loaded.ThreadsManaged.PlatformApps[0].PlatformAppKey)
	assert.Equal(t, uint64(1), loaded.ThreadsManaged.PlatformApps[0].ConfigurationGeneration)
}

func TestLoad_ManagedThreadsPlatformAppIsEnvironmentOnlyCompatible(t *testing.T) {
	t.Setenv("WHATOMATE_APP__ENVIRONMENT", "staging")
	t.Setenv("WHATOMATE_THREADS_MANAGED__ENABLED", "true")
	t.Setenv("WHATOMATE_THREADS_MANAGED__ALLOWED_ORGANIZATION_IDS", managedClinicOrganizationID)
	t.Setenv("WHATOMATE_THREADS_MANAGED__COMPLIANCE_ORGANIZATION_ID", managedComplianceOrganizationID)
	t.Setenv("WHATOMATE_THREADS_MANAGED__REREPLY_BASE_URL", "https://threads.test.example")
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__PLATFORM_APP_KEY", "primary")
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__APP_ID", "123456789012345")
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__APP_SECRET", managedAppSecret)
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__WEBHOOK_VERIFY_TOKEN", managedWebhookVerifyToken)
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__APP_REVIEW_STATUS", "approved")
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__APP_REVIEW_EVIDENCE_SHA256", strings.Repeat("a", 64))
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__APP_REVIEW_APPROVED_AT", "2026-01-02T03:04:05Z")
	t.Setenv("WHATOMATE_THREADS_MANAGED__PLATFORM_APP__CONFIGURATION_GENERATION", "1")

	loaded, err := config.Load("")
	require.NoError(t, err)
	require.Len(t, loaded.ThreadsManaged.PlatformApps, 1)
	assert.Equal(t, "primary", loaded.ThreadsManaged.PlatformApps[0].PlatformAppKey)
	assert.Equal(t, "123456789012345", loaded.ThreadsManaged.PlatformApps[0].AppID)
	assert.Equal(t, uint64(1), loaded.ThreadsManaged.PlatformApps[0].ConfigurationGeneration)
	assert.Empty(t, loaded.ThreadsManaged.PlatformApp, "the env-only object must canonicalize into the runtime slice")
}

func TestManagedThreadsConfigRejectsPartialOrUnsafeReleasePolicy(t *testing.T) {
	valid := approvedManagedThreadsConfig()
	for _, testCase := range []struct {
		name    string
		mutate  func(*config.ThreadsManagedConfig)
		env     string
		message string
	}{
		{
			name: "configured while disabled",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.Enabled = false
			},
			env: "staging", message: "explicitly enabled",
		},
		{
			name: "no release policy",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.AllowedOrganizationIDs = ""
			},
			env: "staging", message: "exactly one",
		},
		{
			name: "allow all outside production",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.AllowedOrganizationIDs = ""
				value.AllowAllOrganizations = true
			},
			env: "staging", message: "explicit production release",
		},
		{
			name: "clinic used as compliance tenant",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.ComplianceOrganizationID = managedClinicOrganizationID
			},
			env: "staging", message: "separate",
		},
		{
			name: "nil compliance tenant",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.ComplianceOrganizationID = "00000000-0000-0000-0000-000000000000"
			},
			env: "staging", message: "canonical UUID",
		},
		{
			name: "duplicate app key",
			mutate: func(value *config.ThreadsManagedConfig) {
				duplicate := value.PlatformApps[0]
				duplicate.AppID = "987654321098765"
				value.PlatformApps = append(value.PlatformApps, duplicate)
			},
			env: "staging", message: "app_key values must be unique",
		},
		{
			name: "secret has whitespace",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.PlatformApps[0].AppSecret += " "
			},
			env: "staging", message: "without surrounding whitespace",
		},
		{
			name: "credential reuse",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.PlatformApps[0].WebhookVerifyToken = value.PlatformApps[0].AppSecret
			},
			env: "staging", message: "distinct",
		},
		{
			name: "approved without evidence",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.PlatformApps[0].AppReviewEvidenceSHA256 = ""
			},
			env: "staging", message: "evidence digest",
		},
		{
			name: "production pending",
			mutate: func(value *config.ThreadsManagedConfig) {
				value.PlatformApps[0].AppReviewStatus = "pending"
				value.PlatformApps[0].AppReviewEvidenceSHA256 = ""
				value.PlatformApps[0].AppReviewApprovedAt = ""
			},
			env: "production", message: "requires approved app review",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := valid
			candidate.PlatformApps = append([]config.ThreadsPlatformAppConfig(nil), valid.PlatformApps...)
			testCase.mutate(&candidate)
			if testCase.env == "production" {
				candidate.ReReplyBaseURL = "https://app.rereply.app"
			}
			err := config.ValidateThreadsManagedConfig(candidate, config.ThreadsAppReviewConfig{}, testCase.env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
			assert.NotContains(t, err.Error(), managedAppSecret)
			assert.NotContains(t, err.Error(), managedWebhookVerifyToken)
		})
	}
}

func TestManagedThreadsNonProductionReviewGateIsExact(t *testing.T) {
	managed := approvedManagedThreadsConfig()
	managed.PlatformApps[0].AppReviewStatus = "pending"
	managed.PlatformApps[0].AppReviewEvidenceSHA256 = ""
	managed.PlatformApps[0].AppReviewApprovedAt = ""
	development := config.ThreadsAppReviewConfig{
		DevelopmentTestingEnabled: true,
		DevelopmentOrganizationID: managedClinicOrganizationID,
		DevelopmentAppID:          managed.PlatformApps[0].AppID,
		DevelopmentProfileID:      "987654321098765",
	}
	require.NoError(t, config.ValidateThreadsManagedConfig(managed, development, "staging"))

	development.DevelopmentAppID = "999999999999999"
	err := config.ValidateThreadsManagedConfig(managed, development, "staging")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact nonproduction app-role")
}

func TestManagedThreadsBaseURLIsDeploymentOwnedCanonicalOrigin(t *testing.T) {
	valid := approvedManagedThreadsConfig()
	for _, candidate := range []string{
		"http://threads.test.example",
		" https://threads.test.example",
		"https://threads.test.example ",
		"https://user@threads.test.example",
		"https://threads.test.example/callback",
		"https://threads.test.example?tenant=clinic",
		"https://threads.test.example#fragment",
		"https://threads.test.example/",
	} {
		t.Run(candidate, func(t *testing.T) {
			managed := valid
			managed.ReReplyBaseURL = candidate
			err := config.ValidateThreadsManagedConfig(
				managed,
				config.ThreadsAppReviewConfig{},
				"staging",
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "canonical HTTPS origin")
		})
	}

	production := valid
	production.ReReplyBaseURL = "https://app.rereply.app"
	require.NoError(t, config.ValidateThreadsManagedConfig(
		production,
		config.ThreadsAppReviewConfig{},
		"production",
	))
	production.ReReplyBaseURL = "https://other.rereply.app"
	err := config.ValidateThreadsManagedConfig(
		production,
		config.ThreadsAppReviewConfig{},
		"production",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical HTTPS origin")
}

func TestManagedThreadsDeploymentSecretsAreExcludedFromJSON(t *testing.T) {
	managed := approvedManagedThreadsConfig()
	encoded, err := json.Marshal(managed)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), managedAppSecret)
	assert.NotContains(t, string(encoded), managedWebhookVerifyToken)
	assert.NotContains(t, string(encoded), strings.Repeat("a", 64))
	assert.Contains(t, string(encoded), `"platform_app_key":"primary"`)
}
