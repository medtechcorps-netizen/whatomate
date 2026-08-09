package metarelay

import (
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
)

const validAccountsJSON = `[
  {
    "key": "page",
    "organization_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
    "meta_business_id": "200000000000001",
    "channel": "messenger",
    "external_account_id": "100000000000010",
    "rereply_webhook_url": "https://rereply.example.test/api/webhooks/channels/00000000-0000-4000-8000-000000000001",
    "access_token_env": "PAGE_TOKEN",
    "rereply_inbound_secret_env": "PAGE_INBOUND",
    "rereply_outbound_secret_env": "PAGE_OUTBOUND"
  },
  {
    "key": "ig-direct",
    "organization_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2",
    "meta_business_id": "200000000000002",
    "channel": "instagram",
    "external_account_id": "17841400000000001",
    "instagram_api_mode": "instagram_login",
    "rereply_webhook_url": "https://rereply.example.test/api/webhooks/channels/00000000-0000-4000-8000-000000000002",
    "access_token_env": "IG_TOKEN",
    "rereply_inbound_secret_env": "IG_INBOUND",
    "rereply_outbound_secret_env": "IG_OUTBOUND"
  }
]`

func validConfigEnvironment() map[string]string {
	reviewedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	return map[string]string{
		"META_RELAY_REDIS_URL":                        "redis://localhost:6379/0",
		"META_RELAY_REREPLY_BASE_URL":                 "https://rereply.example.test",
		"META_RELAY_REREPLY_PROVIDER_PROOF_SECRET":    "config-meta-provider-proof-secret-at-least-32-bytes",
		"META_RELAY_MESSENGER_APP_SECRET":             "parent-secret",
		"META_RELAY_MESSENGER_APP_ID":                 "100000000000001",
		"META_RELAY_MESSENGER_APP_MODE":               "live",
		"META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID":  "300000000000001",
		"META_RELAY_MESSENGER_TECH_PROVIDER_STATUS":   "verified",
		"META_RELAY_MESSENGER_APP_REVIEW_STATUS":      "approved",
		"META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS": "pages_messaging,pages_manage_metadata",
		"META_RELAY_MESSENGER_REVIEWED_BY":            "Meta Governance Reviewer <governance@example.test>",
		"META_RELAY_MESSENGER_REVIEWED_AT":            reviewedAt,
		"META_RELAY_MESSENGER_REVIEW_EVIDENCE":        "https://evidence.example.test/meta/messenger",
		"META_RELAY_MESSENGER_VERIFY_TOKEN":           "parent-verify",
		"META_RELAY_INSTAGRAM_APP_SECRET":             "instagram-secret",
		"META_RELAY_INSTAGRAM_APP_ID":                 "100000000000002",
		"META_RELAY_INSTAGRAM_APP_MODE":               "live",
		"META_RELAY_INSTAGRAM_APP_OWNER_BUSINESS_ID":  "300000000000001",
		"META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS":   "verified",
		"META_RELAY_INSTAGRAM_APP_REVIEW_STATUS":      "approved",
		"META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS": "instagram_business_basic,instagram_business_manage_messages",
		"META_RELAY_INSTAGRAM_REVIEWED_BY":            "Meta Governance Reviewer <governance@example.test>",
		"META_RELAY_INSTAGRAM_REVIEWED_AT":            reviewedAt,
		"META_RELAY_INSTAGRAM_REVIEW_EVIDENCE":        "https://evidence.example.test/meta/instagram",
		"META_RELAY_INSTAGRAM_VERIFY_TOKEN":           "instagram-verify",
		"META_RELAY_GRAPH_API_VERSION":                "v25.0",
		"META_RELAY_ACCOUNTS_JSON":                    validAccountsJSON,
		"PAGE_TOKEN":                                  "page-token-value",
		"PAGE_INBOUND":                                "page-inbound-value-at-least-thirty-two-bytes",
		"PAGE_OUTBOUND":                               "page-outbound-value-at-least-thirty-two-bytes",
		"IG_TOKEN":                                    "ig-token-value",
		"IG_INBOUND":                                  "ig-inbound-value-at-least-thirty-two-bytes",
		"IG_OUTBOUND":                                 "ig-outbound-value-at-least-thirty-two-bytes",
	}
}

func loadTestEnvironment(environment map[string]string) (*Config, error) {
	return loadConfig(func(name string) string { return environment[name] })
}

func TestLoadConfigRequiresDistinctWebhookCredentialsAndExplicitModes(t *testing.T) {
	environment := validConfigEnvironment()
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("load valid config: %v", err)
	}
	if config.MessengerAppSecret != "parent-secret" ||
		config.InstagramLoginAppSecret != "instagram-secret" {
		t.Fatal("webhook app credentials were not loaded independently")
	}
	if config.MessengerAppMode != "live" || config.InstagramAppMode != "live" {
		t.Fatal("both Meta applications must be explicitly recorded in Live mode")
	}
	if config.ReReplyProviderProofSecret != environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"] {
		t.Fatal("deployment provider proof secret was not loaded")
	}
	if config.MessengerAppOwnerBusinessID != config.InstagramAppOwnerBusinessID {
		t.Fatal("the two distinct apps may share one verified owner Business Portfolio")
	}
	if account, ok := config.account(models.ChannelInstagram, "17841400000000001"); !ok ||
		account.InstagramAPIMode != InstagramAPIModeInstagramLogin {
		t.Fatal("Instagram API mode was not indexed")
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_INSTAGRAM_APP_SECRET"] = environment["META_RELAY_MESSENGER_APP_SECRET"]
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("expected duplicate app secret rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_INSTAGRAM_VERIFY_TOKEN"] = environment["META_RELAY_MESSENGER_VERIFY_TOKEN"]
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("expected duplicate verify token rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_INSTAGRAM_APP_ID"] = environment["META_RELAY_MESSENGER_APP_ID"]
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "app IDs must be distinct") {
		t.Fatalf("expected duplicate app ID rejection, got %v", err)
	}
}

func TestLoadConfigRequiresLiveAppModes(t *testing.T) {
	for _, name := range []string{
		"META_RELAY_MESSENGER_APP_MODE",
		"META_RELAY_INSTAGRAM_APP_MODE",
	} {
		t.Run(name, func(t *testing.T) {
			environment := validConfigEnvironment()
			environment[name] = "development"
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), "must be live") {
				t.Fatalf("expected non-Live app rejection, got %v", err)
			}
		})
	}
}

func TestLoadConfigRequiresStrongDistinctProviderAndAccountHMACSecrets(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "missing deployment provider proof",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"] = ""
			},
			want: "at least 32 bytes",
		},
		{
			name: "short account inbound HMAC",
			mutate: func(environment map[string]string) {
				environment["PAGE_INBOUND"] = "short"
			},
			want: "at least 32 bytes",
		},
		{
			name: "provider proof reuses app secret",
			mutate: func(environment map[string]string) {
				shared := "shared-app-and-provider-proof-secret-at-least-32-bytes"
				environment["META_RELAY_MESSENGER_APP_SECRET"] = shared
				environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"] = shared
			},
			want: "distinct from Messenger app secret",
		},
		{
			name: "provider proof reuses tenant HMAC",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"] = environment["PAGE_INBOUND"]
			},
			want: "must differ from the deployment Meta provider proof secret",
		},
		{
			name: "provider proof reuses access token",
			mutate: func(environment map[string]string) {
				environment["PAGE_TOKEN"] = environment["META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"]
			},
			want: "access token must differ",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validConfigEnvironment()
			testCase.mutate(environment)
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load config error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestLoadConfigRequiresSeparateVerifiedAppOwnershipEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		environment string
		value       string
		want        string
	}{
		{
			name:        "numeric app owner",
			environment: "META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID",
			value:       "not-numeric",
			want:        "APP_OWNER_BUSINESS_ID",
		},
		{
			name:        "verified tech provider",
			environment: "META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS",
			value:       "VERIFIED",
			want:        "must be verified",
		},
		{
			name:        "reviewer",
			environment: "META_RELAY_MESSENGER_REVIEWED_BY",
			value:       "reviewer-without-email",
			want:        "Name <email> format",
		},
		{
			name:        "UTC review timestamp",
			environment: "META_RELAY_INSTAGRAM_REVIEWED_AT",
			value:       "2026-08-06T09:02:03+08:00",
			want:        "UTC RFC3339",
		},
		{
			name:        "HTTPS review evidence",
			environment: "META_RELAY_MESSENGER_REVIEW_EVIDENCE",
			value:       "http://evidence.example.test/meta/messenger",
			want:        "HTTPS URL",
		},
		{
			name:        "immutable review evidence URL",
			environment: "META_RELAY_INSTAGRAM_REVIEW_EVIDENCE",
			value:       "https://evidence.example.test/meta/instagram?record=other",
			want:        "query, or fragment",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validConfigEnvironment()
			environment[testCase.environment] = testCase.value
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected %s rejection, got %v", testCase.name, err)
			}
		})
	}
}

func TestLoadConfigRequiresCanonicalOrganizationAndMetaBusinessOwnership(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		oldValue string
		newValue string
		want     string
	}{
		{
			name:     "canonical organization UUID",
			oldValue: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
			newValue: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAA1",
			want:     "organization_id",
		},
		{
			name:     "numeric Meta Business Portfolio",
			oldValue: `"meta_business_id": "200000000000001"`,
			newValue: `"meta_business_id": "not-numeric"`,
			want:     "meta_business_id",
		},
		{
			name:     "numeric Meta asset",
			oldValue: `"external_account_id": "100000000000010"`,
			newValue: `"external_account_id": "page-name"`,
			want:     "external_account_id",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validConfigEnvironment()
			environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
				validAccountsJSON,
				testCase.oldValue,
				testCase.newValue,
				1,
			)
			if _, err := loadTestEnvironment(environment); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected %s rejection, got %v", testCase.name, err)
			}
		})
	}
}

func TestLoadConfigPinsReReplyOriginAndRejectsAmbiguousMappings(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "missing trusted ReReply origin",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REREPLY_BASE_URL"] = ""
			},
			want: "META_RELAY_REREPLY_BASE_URL",
		},
		{
			name: "wrong webhook origin",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(validAccountsJSON, "https://rereply.example.test/", "https://attacker.example/", 1)
			},
			want: "production origin",
		},
		{
			name: "duplicate channel account ID",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(validAccountsJSON, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001", 1)
			},
			want: "reuse ReReply channel account ID",
		},
		{
			name: "duplicate HMAC environment name",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(validAccountsJSON, `"rereply_inbound_secret_env": "IG_INBOUND"`, `"rereply_inbound_secret_env": "PAGE_INBOUND"`, 1)
			},
			want: "reuses account-scoped HMAC environment",
		},
		{
			name: "duplicate HMAC value",
			mutate: func(environment map[string]string) {
				environment["IG_INBOUND"] = environment["PAGE_INBOUND"]
			},
			want: "reuses account-scoped HMAC secret material",
		},
		{
			name: "one organization multiple businesses",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(validAccountsJSON, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", 1)
			},
			want: "multiple Meta Business IDs",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validConfigEnvironment()
			testCase.mutate(environment)
			if _, err := loadTestEnvironment(environment); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load config error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestLoadConfigRequiresCurrentGovernanceReview(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		reviewedAt time.Time
		want       string
	}{
		{name: "future dated", reviewedAt: time.Now().UTC().Add(time.Hour), want: "future-dated"},
		{name: "stale", reviewedAt: time.Now().UTC().Add(-maxGovernanceReviewAge - time.Hour), want: "stale"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			environment := validConfigEnvironment()
			environment["META_RELAY_MESSENGER_REVIEWED_AT"] = testCase.reviewedAt.Format(time.RFC3339)
			if _, err := loadTestEnvironment(environment); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("load config error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestLoadConfigRequiresApprovedAppReviewAndExactProductionMapping(t *testing.T) {
	environment := validConfigEnvironment()
	environment["META_RELAY_MESSENGER_APP_REVIEW_STATUS"] = "pending"
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "must be approved") {
		t.Fatalf("expected App Review gate rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS"] = "pages_manage_metadata"
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "pages_messaging") {
		t.Fatalf("expected missing reviewed permission rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
		validAccountsJSON,
		"/api/webhooks/channels/00000000-0000-4000-8000-000000000001",
		"/some-other-webhook",
		1,
	)
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "/api/webhooks/channels/") {
		t.Fatalf("expected missing production mapping rejection, got %v", err)
	}
}

func TestReReplyChannelAccountIDRequiresExactUUIDWebhookPath(t *testing.T) {
	const accountID = "00000000-0000-4000-8000-000000000123"
	parsed, err := reReplyChannelAccountID(
		"https://rereply.example.test/api/webhooks/channels/" + accountID,
	)
	if err != nil || parsed != accountID {
		t.Fatalf("parse exact webhook path = %q, %v", parsed, err)
	}

	for _, rawURL := range []string{
		"https://rereply.example.test/api/webhooks/channels/not-a-uuid",
		"https://rereply.example.test/api/webhooks/channels/00000000-0000-0000-0000-000000000000",
		"https://rereply.example.test/api/webhooks/channels/" + accountID + "/extra",
		"https://rereply.example.test/api/webhooks/channels/" + accountID + "?workspace=other",
		"https://rereply.example.test/some-other-webhook/" + accountID,
	} {
		if parsed, err := reReplyChannelAccountID(rawURL); err == nil {
			t.Fatalf("unexpectedly accepted %q as %q", rawURL, parsed)
		}
	}
}

func TestLoadConfigRejectsImplicitOrMismatchedInstagramMode(t *testing.T) {
	environment := validConfigEnvironment()
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
		validAccountsJSON,
		`"instagram_api_mode": "instagram_login",`,
		"",
		1,
	)
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "instagram_api_mode") {
		t.Fatalf("expected missing Instagram mode rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
		validAccountsJSON,
		`"channel": "messenger",`,
		`"channel": "messenger", "instagram_api_mode": "facebook_login",`,
		1,
	)
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "only valid for Instagram") {
		t.Fatalf("expected Messenger mode rejection, got %v", err)
	}
}

func TestLoadConfigEnforcesWorkerLeaseInvariant(t *testing.T) {
	environment := validConfigEnvironment()
	environment["META_RELAY_FORWARD_TIMEOUT"] = "15s"
	environment["META_RELAY_PROCESSING_LEASE"] = "20s"
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "must cover") {
		t.Fatalf("expected unsafe lease rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_WORKER_CONCURRENCY"] = "65"
	if _, err := loadTestEnvironment(environment); err == nil ||
		!strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("expected excessive concurrency rejection, got %v", err)
	}
}

func TestLoadConfigErrorsDoNotExposeCredentialValues(t *testing.T) {
	const secret = "do-not-print-this-secret"

	environment := validConfigEnvironment()
	environment["META_RELAY_REDIS_URL"] = "redis://:" + secret + "@%"
	_, err := loadTestEnvironment(environment)
	if err == nil {
		t.Fatal("expected invalid Redis URL")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Redis config error exposed a credential: %q", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_ACCOUNTS_JSON"] = `"` + secret + `"`
	_, err = loadTestEnvironment(environment)
	if err == nil {
		t.Fatal("expected invalid accounts JSON")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("accounts config error exposed source data: %q", err)
	}
}
