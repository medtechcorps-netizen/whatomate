package metarelay

import (
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
)

const validAccountsJSON = `[
  {
    "key": "page",
    "channel": "messenger",
    "external_account_id": "page-1",
    "rereply_webhook_url": "https://rereply.example.test/meta",
    "access_token_env": "PAGE_TOKEN",
    "rereply_inbound_secret_env": "PAGE_INBOUND",
    "rereply_outbound_secret_env": "PAGE_OUTBOUND"
  },
  {
    "key": "ig-direct",
    "channel": "instagram",
    "external_account_id": "ig-1",
    "instagram_api_mode": "instagram_login",
    "rereply_webhook_url": "https://rereply.example.test/meta",
    "access_token_env": "IG_TOKEN",
    "rereply_inbound_secret_env": "IG_INBOUND",
    "rereply_outbound_secret_env": "IG_OUTBOUND"
  }
]`

func validConfigEnvironment() map[string]string {
	return map[string]string{
		"META_RELAY_REDIS_URL":              "redis://localhost:6379/0",
		"META_RELAY_MESSENGER_APP_SECRET":   "parent-secret",
		"META_RELAY_MESSENGER_VERIFY_TOKEN": "parent-verify",
		"META_RELAY_INSTAGRAM_APP_SECRET":   "instagram-secret",
		"META_RELAY_INSTAGRAM_VERIFY_TOKEN": "instagram-verify",
		"META_RELAY_GRAPH_API_VERSION":      "v25.0",
		"META_RELAY_ACCOUNTS_JSON":          validAccountsJSON,
		"PAGE_TOKEN":                        "page-token-value",
		"PAGE_INBOUND":                      "page-inbound-value",
		"PAGE_OUTBOUND":                     "page-outbound-value",
		"IG_TOKEN":                          "ig-token-value",
		"IG_INBOUND":                        "ig-inbound-value",
		"IG_OUTBOUND":                       "ig-outbound-value",
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
	if account, ok := config.account(models.ChannelInstagram, "ig-1"); !ok ||
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

func TestLoadConfigRequiresExplicitDynamicRegistryAndReaderFence(t *testing.T) {
	environment := validConfigEnvironment()
	delete(environment, "META_RELAY_ACCOUNTS_JSON")
	environment["META_RELAY_REGISTRY_URL"] = "https://app.example.test/internal/meta-registry/v1/resolve"
	environment["META_RELAY_REGISTRY_SECRET"] = "synthetic-registry-config-secret-at-least-32-bytes"
	environment["META_RELAY_REGISTRY_EDGE_SECRET"] = "synthetic-registry-edge-secret-at-least-32-bytes"
	environment["META_RELAY_MANAGED_MESSENGER_APP_ID"] = "123456"
	environment["META_RELAY_MANAGED_MESSENGER_APP_SECRET"] = "managed-messenger-app-secret-at-least-32-bytes"
	environment["META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN"] = "managed-messenger-verify-token"
	if _, err := loadTestEnvironment(environment); err == nil || !strings.Contains(err.Error(), "exactly when") {
		t.Fatalf("expected explicit registry gate, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["META_RELAY_REGISTRY_URL"] = "https://app.example.test/internal/meta-registry/v1/resolve"
	environment["META_RELAY_REGISTRY_SECRET"] = "synthetic-registry-config-secret-at-least-32-bytes"
	environment["META_RELAY_REGISTRY_EDGE_SECRET"] = "synthetic-registry-edge-secret-at-least-32-bytes"
	environment["META_RELAY_MANAGED_MESSENGER_APP_ID"] = "123456"
	environment["META_RELAY_MANAGED_MESSENGER_APP_SECRET"] = "managed-messenger-app-secret-at-least-32-bytes"
	environment["META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN"] = "managed-messenger-verify-token"
	environment["META_RELAY_REGISTRY_ENABLED"] = "true"
	if _, err := loadTestEnvironment(environment); err == nil || !strings.Contains(err.Error(), "READER_VERSION") {
		t.Fatalf("expected reader version gate, got %v", err)
	}

	environment["META_RELAY_DYNAMIC_QUEUE_READER_VERSION"] = "2"
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("expected explicitly fenced mixed static/dynamic config, got %v", err)
	}
	if !config.RegistryEnabled || config.RegistryQueueReader != InboundJobSchemaVersion {
		t.Fatalf("dynamic registry fence was not retained: %#v", config)
	}

	delete(environment, "META_RELAY_MANAGED_MESSENGER_APP_ID")
	if _, err := loadTestEnvironment(environment); err == nil || !strings.Contains(err.Error(), "MANAGED_MESSENGER_APP_ID") {
		t.Fatalf("expected exact dynamic Messenger app identity, got %v", err)
	}
}
