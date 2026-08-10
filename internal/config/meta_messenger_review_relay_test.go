package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reviewConfigAuthSecret       = "review-config-auth-secret-at-least-thirty-two-bytes"
	reviewConfigWrapSecret       = "review-config-wrap-secret-at-least-thirty-two-bytes"
	reviewConfigProofSecret      = "review-config-provider-proof-at-least-thirty-two-bytes"
	reviewConfigAppSecret        = "review-config-meta-app-secret-at-least-thirty-two-bytes"
	reviewConfigEncryptionSecret = "review-config-encryption-key-at-least-thirty-two-bytes"
)

func TestValidateMetaMessengerReviewRelayConfigAcceptsExactStagingAuthority(t *testing.T) {
	review, onboarding, relay, app := validMetaMessengerReviewRelayConfig()
	require.NoError(t, validateMetaMessengerReviewRelayConfig(review, onboarding, relay, app))
}

func TestValidateMetaMessengerReviewRelayConfigFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MetaMessengerReviewRelayConfig, *MetaMessengerOnboardingConfig, *MetaRelayConfig, *AppConfig)
	}{
		{name: "disabled partial config", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.Enabled = false
		}},
		{name: "development", mutate: func(_ *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, app *AppConfig) {
			app.Environment = "development"
		}},
		{name: "production", mutate: func(_ *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, app *AppConfig) {
			app.Environment = "production"
		}},
		{name: "environment case", mutate: func(_ *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, app *AppConfig) {
			app.Environment = "Staging"
		}},
		{name: "wrong mode", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.Mode = "production"
		}},
		{name: "onboarding disabled", mutate: func(_ *MetaMessengerReviewRelayConfig, onboarding *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			onboarding.Enabled = false
		}},
		{name: "zero account UUID", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.ChannelAccountID = uuid.Nil.String()
		}},
		{name: "missing generation", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.Generation = ""
		}},
		{name: "leading zero Page", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.PageID = "012345"
		}},
		{name: "wrong app ID", mutate: func(_ *MetaMessengerReviewRelayConfig, onboarding *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			onboarding.AppID = "not-numeric"
		}},
		{name: "expired authority", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.ExpiresAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
		}},
		{name: "noncanonical expiry", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.ExpiresAt = time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05+00:00")
		}},
		{name: "relay mismatch", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.RelayBaseURL = "https://other-relay.example.test/meta"
		}},
		{name: "relay trailing slash", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.RelayBaseURL += "/"
		}},
		{name: "ReReply path", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.ReReplyBaseURL += "/api"
		}},
		{name: "ReReply loopback", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.ReReplyBaseURL = "https://127.0.0.1"
		}},
		{name: "short auth secret", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.BrokerAuthSecret = "short"
		}},
		{name: "wrap whitespace", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.BrokerWrapSecret += " "
		}},
		{name: "auth embedded whitespace", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.BrokerAuthSecret = review.BrokerAuthSecret[:20] + "\n" + review.BrokerAuthSecret[20:]
		}},
		{name: "auth equals wrap", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			review.BrokerWrapSecret = review.BrokerAuthSecret
		}},
		{name: "proof equals Meta app", mutate: func(review *MetaMessengerReviewRelayConfig, onboarding *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, _ *AppConfig) {
			onboarding.AppSecret = review.ProviderProofSecret
		}},
		{name: "wrap equals encryption", mutate: func(review *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, _ *MetaRelayConfig, app *AppConfig) {
			app.EncryptionKey = review.BrokerWrapSecret
		}},
		{name: "mixed production relay", mutate: func(_ *MetaMessengerReviewRelayConfig, _ *MetaMessengerOnboardingConfig, relay *MetaRelayConfig, _ *AppConfig) {
			relay.BaseURL = "https://production-relay.example.test/meta"
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			review, onboarding, relay, app := validMetaMessengerReviewRelayConfig()
			testCase.mutate(&review, &onboarding, &relay, &app)
			assert.Error(t, validateMetaMessengerReviewRelayConfig(review, onboarding, relay, app))
		})
	}
}

func TestLoadMapsMetaMessengerReviewRelayEnvironment(t *testing.T) {
	review, _, _, _ := validMetaMessengerReviewRelayConfig()
	environment := map[string]string{
		"WHATOMATE_APP__ENVIRONMENT":                                   "staging",
		"WHATOMATE_APP__ENCRYPTION_KEY":                                reviewConfigEncryptionSecret,
		"WHATOMATE_META_RELAY__BASE_URL":                               "",
		"WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON":                 "",
		"WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET":                  "",
		"WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED":                 "true",
		"WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID":                  "1035383549213572",
		"WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET":              reviewConfigAppSecret,
		"WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL":  review.RelayBaseURL,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__ENABLED":               "true",
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__MODE":                  metareview.Mode,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__ORGANIZATION_ID":       review.OrganizationID,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__META_BUSINESS_ID":      review.MetaBusinessID,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__PAGE_ID":               review.PageID,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__CHANNEL_ACCOUNT_ID":    review.ChannelAccountID,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__GENERATION":            review.Generation,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__EXPIRES_AT":            review.ExpiresAt,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__RELAY_BASE_URL":        review.RelayBaseURL,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__REREPLY_BASE_URL":      review.ReReplyBaseURL,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_AUTH_SECRET":    reviewConfigAuthSecret,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__BROKER_WRAP_SECRET":    reviewConfigWrapSecret,
		"WHATOMATE_META_MESSENGER_REVIEW_RELAY__PROVIDER_PROOF_SECRET": reviewConfigProofSecret,
	}
	for key, value := range environment {
		t.Setenv(key, value)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, review, cfg.MetaMessengerReviewRelay)
}

func TestLoadRejectsReviewConfigurationOutsideExactStaging(t *testing.T) {
	review, onboarding, relay, app := validMetaMessengerReviewRelayConfig()
	app.Environment = "production"
	err := validateMetaMessengerReviewRelayConfig(review, onboarding, relay, app)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "exactly staging"))
}

func validMetaMessengerReviewRelayConfig() (
	MetaMessengerReviewRelayConfig,
	MetaMessengerOnboardingConfig,
	MetaRelayConfig,
	AppConfig,
) {
	relayBase := "https://review-relay.example.test/meta"
	review := MetaMessengerReviewRelayConfig{
		Enabled:             true,
		Mode:                metareview.Mode,
		OrganizationID:      "c73f761f-5154-4fe1-9a13-06bae570277a",
		MetaBusinessID:      "3852210034910979",
		PageID:              "1038752885977372",
		ChannelAccountID:    "88dadf4a-7ea5-4b42-9a8c-174b3db4a73c",
		Generation:          "9bd97a61-4388-4430-8076-1f60c76e44d7",
		ExpiresAt:           time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
		RelayBaseURL:        relayBase,
		ReReplyBaseURL:      "https://staging-rereply.example.test",
		BrokerAuthSecret:    reviewConfigAuthSecret,
		BrokerWrapSecret:    reviewConfigWrapSecret,
		ProviderProofSecret: reviewConfigProofSecret,
	}
	onboarding := MetaMessengerOnboardingConfig{
		Enabled:             true,
		AppID:               "1035383549213572",
		AppSecret:           reviewConfigAppSecret,
		TrustedRelayBaseURL: relayBase,
	}
	relay := MetaRelayConfig{}
	app := AppConfig{
		Environment:   "staging",
		EncryptionKey: reviewConfigEncryptionSecret,
	}
	return review, onboarding, relay, app
}
