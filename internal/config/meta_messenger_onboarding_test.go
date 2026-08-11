package config_test

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaMessengerOnboardingDefaultsToDisabledMedtechContract(t *testing.T) {
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "false")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "")
	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.False(t, cfg.MetaMessengerOnboarding.Enabled)
	assert.Empty(t, cfg.MetaMessengerOnboarding.AppID)
	assert.Empty(t, cfg.MetaMessengerOnboarding.AppSecret)
	assert.Equal(t, "1720929458946813", cfg.MetaMessengerOnboarding.ConfigID)
	assert.Equal(t, "2018290039073161", cfg.MetaMessengerOnboarding.OwnerBusinessID)
	assert.Equal(t, "v25.0", cfg.MetaMessengerOnboarding.GraphAPIVersion)
	assert.Equal(t, "https://graph.facebook.com", cfg.MetaMessengerOnboarding.GraphBaseURL)
}

func TestMetaMessengerOnboardingEnabledRequiresServerOnlyDeploymentValues(t *testing.T) {
	setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "")

	_, err := config.Load("")
	require.Error(t, err)
	assert.ErrorContains(t, err, "APP_ID is required")
}

func TestMetaMessengerOnboardingMapsMedtechDeploymentEnvironment(t *testing.T) {
	setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "100000000000001")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "server-only-medtech-app-secret")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "https://relay.example.test/meta")

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.True(t, cfg.MetaMessengerOnboarding.Enabled)
	assert.Equal(t, "100000000000001", cfg.MetaMessengerOnboarding.AppID)
	assert.Equal(t, "server-only-medtech-app-secret", cfg.MetaMessengerOnboarding.AppSecret)
	assert.Equal(t, "1720929458946813", cfg.MetaMessengerOnboarding.ConfigID)
	assert.Equal(t, "2018290039073161", cfg.MetaMessengerOnboarding.OwnerBusinessID)
	assert.Equal(t, "v25.0", cfg.MetaMessengerOnboarding.GraphAPIVersion)
	assert.Equal(t, "https://relay.example.test/meta", cfg.MetaMessengerOnboarding.TrustedRelayBaseURL)
}

func setMetaMessengerRelayTestEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("WHATOMATE_META_RELAY__BASE_URL", baseURL)
	t.Setenv("WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON", `{"accounts":[{"test":true}]}`)
	t.Setenv(
		"WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET",
		"test-meta-provider-proof-secret-at-least-32-bytes",
	)
}

func TestMetaMessengerOnboardingRejectsMalformedDisabledAppID(t *testing.T) {
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "false")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "not-a-meta-id")

	_, err := config.Load("")
	require.Error(t, err)
	assert.ErrorContains(t, err, "canonical numeric Meta app ID")
}

func TestMetaMessengerProductionGraphOriginIsExactlyPinned(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		baseURL   string
		wantError bool
	}{
		{name: "exact origin", baseURL: "https://graph.facebook.com"},
		{name: "alternate host", baseURL: "https://graph.example.test", wantError: true},
		{name: "explicit port", baseURL: "https://graph.facebook.com:443", wantError: true},
		{name: "trailing slash", baseURL: "https://graph.facebook.com/", wantError: true},
		{name: "path", baseURL: "https://graph.facebook.com/v25.0", wantError: true},
		{name: "query", baseURL: "https://graph.facebook.com?x=1", wantError: true},
		{name: "fragment", baseURL: "https://graph.facebook.com#x", wantError: true},
		{name: "surrounding whitespace", baseURL: " https://graph.facebook.com ", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("WHATOMATE_APP__ENVIRONMENT", "production")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "false")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_BASE_URL", testCase.baseURL)
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "")
			_, err := config.Load("")
			if testCase.wantError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMetaMessengerOnboardingRequiresProtectedRelayBaseMatch(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		onboardingBaseURL string
		wantError         bool
	}{
		{
			name:              "canonical trailing slash matches",
			onboardingBaseURL: "https://relay.example.test/meta/",
		},
		{
			name:              "different relay path is rejected",
			onboardingBaseURL: "https://relay.example.test/other",
			wantError:         true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
			t.Setenv("WHATOMATE_APP__ENVIRONMENT", "staging")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "100000000000001")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "server-only-medtech-app-secret")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", testCase.onboardingBaseURL)
			_, err := config.Load("")
			if testCase.wantError {
				assert.ErrorContains(t, err, "must match")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMetaMessengerOnboardingIsFailClosedInProduction(t *testing.T) {
	setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
	t.Setenv("WHATOMATE_APP__ENVIRONMENT", "production")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "100000000000001")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "server-only-medtech-app-secret")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "https://relay.example.test/meta")

	_, err := config.Load("")
	require.ErrorContains(t, err, "staging-only")
}
