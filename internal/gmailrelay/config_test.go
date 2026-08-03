package gmailrelay

import (
	"strings"
	"testing"
	"time"
)

func validConfigEnvironment() map[string]string {
	return map[string]string{
		"GMAIL_RELAY_REDIS_URL":            "redis://localhost:6379/0",
		"GMAIL_RELAY_MAILBOX":              "realignphysiolates@gmail.com",
		"GMAIL_RELAY_GOOGLE_CLIENT_ID":     "gmail-relay-client-id",
		"GMAIL_RELAY_GOOGLE_CLIENT_SECRET": "gmail-relay-client-secret",
		"GMAIL_RELAY_GOOGLE_REDIRECT_URL":  "https://relay.example.test/oauth/google/callback",
		"GMAIL_RELAY_ENCRYPTION_KEY":       strings.Repeat("k", 32),
		"GMAIL_RELAY_SETUP_KEY":            strings.Repeat("s", 32),
	}
}

func loadTestConfig(environment map[string]string) (*Config, error) {
	return loadConfig(func(name string) string { return environment[name] })
}

func TestLoadConfigAppliesSecureDefaultsForConfiguredMailbox(t *testing.T) {
	config, err := loadTestConfig(validConfigEnvironment())
	if err != nil {
		t.Fatalf("load valid Gmail relay config: %v", err)
	}
	if config.Mailbox != "realignphysiolates@gmail.com" {
		t.Fatalf("unexpected mailbox %q", config.Mailbox)
	}
	if config.ListenAddr != defaultListenAddr || config.RedisPrefix != defaultRedisPrefix {
		t.Fatal("relay defaults were not applied")
	}
	if config.GoogleAuthURL != defaultGoogleAuthURL ||
		config.GoogleTokenURL != defaultGoogleTokenURL ||
		config.GmailAPIBaseURL != defaultGmailAPIBaseURL {
		t.Fatal("official Google endpoint defaults were not applied")
	}
	if config.HTTPTimeout != 30*time.Second {
		t.Fatalf("unexpected HTTP timeout %s", config.HTTPTimeout)
	}
	if config.Paired() {
		t.Fatal("an unpaired bootstrap configuration must remain valid")
	}
}

func TestLoadConfigAcceptsCompleteReReplyPairingAndSecretFallback(t *testing.T) {
	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_REREPLY_WEBHOOK_URL"] = "https://app.rereply.app/api/webhooks/channels/account-1"
	environment["GMAIL_RELAY_REREPLY_INBOUND_SECRET"] = strings.Repeat("i", 32)

	config, err := loadTestConfig(environment)
	if err != nil {
		t.Fatalf("load paired relay config: %v", err)
	}
	if !config.Paired() || config.ReReplyOutboundSecret != config.ReReplyInboundSecret {
		t.Fatal("paired relay or signing-secret fallback was not configured")
	}
}

func TestLoadConfigRejectsPartialReReplyPairing(t *testing.T) {
	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_REREPLY_WEBHOOK_URL"] = "https://app.rereply.app/api/webhooks/channels/account-1"
	if _, err := loadTestConfig(environment); err == nil {
		t.Fatal("expected partial relay pairing to fail")
	}
}

func TestLoadConfigSupportsAnyNormalizedGoogleMailbox(t *testing.T) {
	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_MAILBOX"] = "Team.Support@Example.com"
	config, err := loadTestConfig(environment)
	if err != nil {
		t.Fatalf("load workspace mailbox: %v", err)
	}
	if config.Mailbox != "team.support@example.com" {
		t.Fatalf("mailbox was not normalized: %q", config.Mailbox)
	}
}

func TestLoadConfigRequiresRedisAndEncryption(t *testing.T) {
	environment := validConfigEnvironment()
	delete(environment, "GMAIL_RELAY_REDIS_URL")
	if _, err := loadTestConfig(environment); err == nil || !strings.Contains(err.Error(), "REDIS_URL is required") {
		t.Fatalf("expected mandatory Redis error, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["GMAIL_RELAY_REDIS_URL"] = "redis://:do-not-print-this@%"
	if _, err := loadTestConfig(environment); err == nil || strings.Contains(err.Error(), "do-not-print-this") {
		t.Fatalf("expected redacted invalid Redis error, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["GMAIL_RELAY_ENCRYPTION_KEY"] = "too-short"
	if _, err := loadTestConfig(environment); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("expected encryption key error, got %v", err)
	}
}

func TestLoadConfigRequiresTLSForRemoteRedis(t *testing.T) {
	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_REDIS_URL"] = "redis://cache.example.test:6379/0"
	if _, err := loadTestConfig(environment); err == nil || !strings.Contains(err.Error(), "must use TLS") {
		t.Fatalf("expected insecure remote Redis rejection, got %v", err)
	}

	environment["GMAIL_RELAY_REDIS_URL"] = "rediss://cache.example.test:25061/0"
	if _, err := loadTestConfig(environment); err != nil {
		t.Fatalf("TLS-protected remote Redis should be accepted: %v", err)
	}
}

func TestLoadConfigRejectsInvalidMailboxAndEndpoints(t *testing.T) {
	for _, mailbox := range []string{"", "Realign Physio <realignphysiolates@gmail.com>", "not-an-address"} {
		environment := validConfigEnvironment()
		environment["GMAIL_RELAY_MAILBOX"] = mailbox
		if _, err := loadTestConfig(environment); err == nil {
			t.Fatalf("expected mailbox %q to be rejected", mailbox)
		}
	}

	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_GOOGLE_REDIRECT_URL"] = "http://relay.example.test/oauth/google/callback"
	if _, err := loadTestConfig(environment); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected insecure redirect rejection, got %v", err)
	}

	environment = validConfigEnvironment()
	environment["GMAIL_RELAY_GOOGLE_REDIRECT_URL"] = "http://127.0.0.1:8082/oauth/google/callback"
	if _, err := loadTestConfig(environment); err != nil {
		t.Fatalf("local development callback should be allowed: %v", err)
	}
}

func TestLoadConfigNeverExposesOAuthSecret(t *testing.T) {
	const secret = "do-not-print-google-client-secret"
	environment := validConfigEnvironment()
	environment["GMAIL_RELAY_GOOGLE_CLIENT_SECRET"] = secret
	environment["GMAIL_RELAY_GOOGLE_AUTH_URL"] = "://bad"
	_, err := loadTestConfig(environment)
	if err == nil {
		t.Fatal("expected invalid Google endpoint")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error exposed client secret: %q", err)
	}
}
