package metarelay

import (
	"strings"
	"testing"
	"time"
)

func validReviewConfigEnvironment() map[string]string {
	return map[string]string{
		"META_RELAY_RUNTIME_MODE":                    "staging_messenger_review",
		"META_RELAY_DEPLOYMENT_ENVIRONMENT":          "staging",
		"META_RELAY_REDIS_URL":                       "redis://localhost:6379/0",
		"META_RELAY_REREPLY_BASE_URL":                "https://staging.rereply.example.test",
		"META_RELAY_REREPLY_PROVIDER_PROOF_SECRET":   "review-provider-proof-secret-at-least-32-bytes",
		"META_RELAY_MESSENGER_APP_SECRET":            "review-messenger-app-secret-at-least-32-bytes",
		"META_RELAY_MESSENGER_APP_ID":                "1035383549213572",
		"META_RELAY_MESSENGER_APP_MODE":              "development",
		"META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID": "300000000000001",
		"META_RELAY_MESSENGER_TECH_PROVIDER_STATUS":  "verified",
		"META_RELAY_MESSENGER_APP_REVIEW_STATUS":     "not_submitted",
		"META_RELAY_MESSENGER_VERIFY_TOKEN":          "review-messenger-verify-token-at-least-32-bytes",
		"META_RELAY_REVIEW_ORGANIZATION_ID":          "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
		"META_RELAY_REVIEW_META_BUSINESS_ID":         "300000000000099",
		"META_RELAY_REVIEW_PAGE_ID":                  "211573738714332",
		"META_RELAY_REVIEW_CHANNEL_ACCOUNT_ID":       "00000000-0000-4000-8000-000000000099",
		"META_RELAY_REVIEW_GENERATION":               "99999999-9999-4999-8999-999999999999",
		"META_RELAY_REVIEW_EXPIRES_AT":               time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
		"META_RELAY_REVIEW_BROKER_AUTH_SECRET":       "review-broker-auth-secret-at-least-thirty-two-bytes",
		"META_RELAY_REVIEW_BROKER_WRAP_SECRET":       "review-broker-wrap-secret-at-least-thirty-two-bytes",
		"META_RELAY_REVIEW_BINDING_CACHE_TTL":        "2s",
	}
}

func TestLoadConfigAcceptsOnlyDynamicMessengerReviewBinding(t *testing.T) {
	config, err := loadTestEnvironment(validReviewConfigEnvironment())
	if err != nil {
		t.Fatalf("load review config: %v", err)
	}
	if !config.stagingMessengerReview() || config.DeploymentEnvironment != "staging" {
		t.Fatalf("unexpected review runtime: mode=%q environment=%q", config.RuntimeMode, config.DeploymentEnvironment)
	}
	if len(config.Accounts) != 0 {
		t.Fatal("review mode must not load a static account inventory")
	}
	if _, ok := config.accountByKey("anything"); ok {
		t.Fatal("review mode must keep the production account index empty")
	}
	if config.InstagramLoginAppID != "" || config.InstagramLoginAppSecret != "" {
		t.Fatal("review mode must not contain an Instagram application")
	}
	expectedPrefix := "rereply:meta-relay:review:" + config.ReviewGeneration + ":"
	if config.RedisPrefix != expectedPrefix {
		t.Fatalf("review Redis prefix=%q, want %q", config.RedisPrefix, expectedPrefix)
	}
}

func TestLoadReviewConfigAcceptsDevelopmentOrLiveWithUnapprovedReviewState(t *testing.T) {
	for _, appMode := range []string{"development", "live"} {
		for _, reviewStatus := range []string{"not_submitted", "pending", "in_review"} {
			t.Run(appMode+"_"+reviewStatus, func(t *testing.T) {
				environment := validReviewConfigEnvironment()
				environment["META_RELAY_MESSENGER_APP_MODE"] = appMode
				environment["META_RELAY_MESSENGER_APP_REVIEW_STATUS"] = reviewStatus

				config, err := loadTestEnvironment(environment)
				if err != nil {
					t.Fatalf("load review config: %v", err)
				}
				if config.MessengerAppMode != appMode || config.MessengerAppReviewStatus != reviewStatus {
					t.Fatalf(
						"loaded app governance = %q/%q, want %q/%q",
						config.MessengerAppMode,
						config.MessengerAppReviewStatus,
						appMode,
						reviewStatus,
					)
				}
			})
		}
	}
}

func TestLoadReviewConfigRejectsProductionAndFalseGovernanceAssertions(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "production deployment",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_DEPLOYMENT_ENVIRONMENT"] = "production"
			},
			want: "must be staging",
		},
		{
			name: "invalid app mode",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_APP_MODE"] = "production"
			},
			want: "must be development or live",
		},
		{
			name: "approved app review assertion",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_APP_REVIEW_STATUS"] = "approved"
			},
			want: "must not claim approval",
		},
		{
			name: "approved permission assertion",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS"] = "pages_messaging"
			},
			want: "must not contain approved permission",
		},
		{
			name: "review evidence assertion",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_REVIEW_EVIDENCE"] = "https://evidence.example.test/not-approved"
			},
			want: "must not contain approved permission",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validReviewConfigEnvironment()
			testCase.mutate(environment)
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load review config error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestLoadReviewConfigRejectsInstagramAndStaticAccounts(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "Instagram app",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_INSTAGRAM_APP_ID"] = "100000000000002"
			},
			want: "instagram configuration is forbidden",
		},
		{
			name: "static account inventory",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = validAccountsJSON
			},
			want: "META_RELAY_ACCOUNTS_JSON is forbidden",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validReviewConfigEnvironment()
			testCase.mutate(environment)
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load review config error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestLoadReviewConfigRejectsInvalidWhitelistAndBrokerSecret(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "noncanonical organization",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_ORGANIZATION_ID"] = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAA1"
			},
			want: "canonical non-zero UUID",
		},
		{
			name: "nonnumeric business",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_META_BUSINESS_ID"] = "business"
			},
			want: "numeric Meta Business",
		},
		{
			name: "nonnumeric Page",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_PAGE_ID"] = "page"
			},
			want: "numeric Meta Page",
		},
		{
			name: "noncanonical channel account",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_CHANNEL_ACCOUNT_ID"] = "not-a-uuid"
			},
			want: "canonical non-zero UUID",
		},
		{
			name: "noncanonical generation",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_GENERATION"] = "NOT-A-UUID"
			},
			want: "canonical non-zero UUID",
		},
		{
			name: "expired authority",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_EXPIRES_AT"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			},
			want: "must be in the future",
		},
		{
			name: "short broker auth secret",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_BROKER_AUTH_SECRET"] = "short"
			},
			want: "at least 32 bytes",
		},
		{
			name: "broker auth secret contains whitespace",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_BROKER_AUTH_SECRET"] = "review broker auth secret with more than thirty two bytes"
			},
			want: "no whitespace",
		},
		{
			name: "reused provider secret",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_BROKER_AUTH_SECRET"] = environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"]
			},
			want: "distinct from review provider proof secret",
		},
		{
			name: "reused wrap secret",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_BROKER_WRAP_SECRET"] = environment["META_RELAY_REVIEW_BROKER_AUTH_SECRET"]
			},
			want: "must be distinct from review broker authentication secret",
		},
		{
			name: "short verify token",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_VERIFY_TOKEN"] = "short"
			},
			want: "at least 32 bytes",
		},
		{
			name: "app secret reused as verify token",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_VERIFY_TOKEN"] = environment["META_RELAY_MESSENGER_APP_SECRET"]
			},
			want: "Messenger verify token must be distinct from Messenger app secret",
		},
		{
			name: "nonisolated Redis prefix",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REDIS_PREFIX"] = defaultRedisPrefix
			},
			want: "must exactly isolate the review generation",
		},
		{
			name: "remote Redis without TLS",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REDIS_URL"] = "redis://cache.example.test:6379/0"
			},
			want: "must use rediss TLS outside loopback",
		},
		{
			name: "excessive cache TTL",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REVIEW_BINDING_CACHE_TTL"] = "11s"
			},
			want: "not exceed 10s",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validReviewConfigEnvironment()
			testCase.mutate(environment)
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load review config error = %v, want %q", err, testCase.want)
			}
		})
	}
}
