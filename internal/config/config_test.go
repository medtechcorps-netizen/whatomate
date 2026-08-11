package config_test

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // matches the coturn TURN REST API derivation under test
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoad_AppliesDefaultsForMissingFields(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, ""))
	require.NoError(t, err)

	assert.Equal(t, "ReReply", cfg.App.Name)
	assert.Equal(t, "development", cfg.App.Environment)
	assert.Equal(t, "0.0.0.0", cfg.Server.Host)
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, 30, cfg.Server.ReadTimeout)
	assert.Equal(t, 30, cfg.Server.WriteTimeout)
	assert.Equal(t, 5432, cfg.Database.Port)
	assert.Equal(t, "rereply_app", cfg.Database.RuntimeRole)
	assert.False(t, cfg.Database.RLSEnabled)
	assert.Equal(t, "disable", cfg.Database.SSLMode)
	assert.Equal(t, 25, cfg.Database.MaxOpenConns)
	assert.Equal(t, 5, cfg.Database.MaxIdleConns)
	assert.Equal(t, 300, cfg.Database.ConnMaxLifetime)
	assert.Equal(t, 6379, cfg.Redis.Port)
	assert.Equal(t, 15, cfg.JWT.AccessExpiryMins)
	assert.Equal(t, 1, cfg.JWT.RefreshExpiryDays)
	assert.Equal(t, "v24.0", cfg.WhatsApp.APIVersion)
	assert.Equal(t, "https://graph.facebook.com", cfg.WhatsApp.BaseURL)
	assert.Equal(t, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", cfg.AI.QwenBaseURL)
	assert.Equal(t, "https://accounts.google.com/o/oauth2/auth", cfg.GoogleSearchConsole.AuthURL)
	assert.Equal(t, "https://oauth2.googleapis.com/token", cfg.GoogleSearchConsole.TokenURL)
	assert.Equal(t, "https://www.googleapis.com/webmasters/v3", cfg.GoogleSearchConsole.APIBaseURL)
	assert.Equal(t, "local", cfg.Storage.Type)
	assert.Equal(t, "./uploads", cfg.Storage.LocalPath)
	assert.Equal(t, "admin@rereply.app", cfg.DefaultAdmin.Email)
	assert.Equal(t, "admin", cfg.DefaultAdmin.Password)
	assert.Equal(t, "ReReply Administrator", cfg.DefaultAdmin.FullName)
}

func TestLoad_FileValuesOverrideDefaults(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[app]
name = "MyApp"
environment = "production"

[server]
port = 9090

[database]
host = "db.example.com"
port = 5433
user = "u"
password = "p"
name = "n"

[whatsapp]
api_version = "v22.0"
`))
	require.NoError(t, err)

	assert.Equal(t, "MyApp", cfg.App.Name)
	assert.Equal(t, "production", cfg.App.Environment)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "db.example.com", cfg.Database.Host)
	assert.Equal(t, 5433, cfg.Database.Port)
	assert.Equal(t, "v22.0", cfg.WhatsApp.APIVersion)
}

func TestLoad_ProductionEnvironmentForcesSecureCookie(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[app]
environment = "production"

[cookie]
secure = false
`))
	require.NoError(t, err)
	assert.True(t, cfg.Cookie.Secure, "production environment must force Cookie.Secure=true")
}

func TestLoad_DevelopmentDoesNotForceSecureCookie(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[app]
environment = "development"

[cookie]
secure = false
`))
	require.NoError(t, err)
	assert.False(t, cfg.Cookie.Secure)
}

func TestLoad_EnvVarsOverrideFile(t *testing.T) {
	t.Setenv("WHATOMATE_DATABASE__HOST", "from-env")
	t.Setenv("WHATOMATE_DATABASE__URL", "postgres://private-db/app")
	t.Setenv("WHATOMATE_DATABASE__MIGRATION_URL", "postgres://private-db/owner")
	t.Setenv("WHATOMATE_DATABASE__RUNTIME_ROLE", "rereply_runtime")
	t.Setenv("WHATOMATE_DATABASE__RLS_ENABLED", "true")
	t.Setenv("WHATOMATE_REDIS__URL", "rediss://private-cache/0")
	t.Setenv("WHATOMATE_SERVER__PORT", "1234")
	t.Setenv("WHATOMATE_GOOGLE_SEARCH_CONSOLE__CLIENT_ID", "gsc-client")
	t.Setenv("WHATOMATE_GOOGLE_SEARCH_CONSOLE__CLIENT_SECRET", "gsc-secret")
	t.Setenv("WHATOMATE_GOOGLE_SEARCH_CONSOLE__REDIRECT_URL", "https://app.example.test/api/integrations/google_search_console/callback")

	cfg, err := config.Load(writeConfig(t, `
[database]
host = "from-file"

[server]
port = 8080
`))
	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.Database.Host, "WHATOMATE_DATABASE__HOST must override file")
	assert.Equal(t, "postgres://private-db/app", cfg.Database.URL)
	assert.Equal(t, "postgres://private-db/owner", cfg.Database.MigrationURL)
	assert.Equal(t, "rereply_runtime", cfg.Database.RuntimeRole)
	assert.True(t, cfg.Database.RLSEnabled)
	assert.Equal(t, "rediss://private-cache/0", cfg.Redis.URL)
	assert.Equal(t, 1234, cfg.Server.Port, "WHATOMATE_SERVER__PORT must override file")
	assert.Equal(t, "gsc-client", cfg.GoogleSearchConsole.ClientID)
	assert.Equal(t, "gsc-secret", cfg.GoogleSearchConsole.ClientSecret)
	assert.Equal(t, "https://app.example.test/api/integrations/google_search_console/callback", cfg.GoogleSearchConsole.RedirectURL)
}

func TestLoad_EmptyConfigPathStillLoadsDefaults(t *testing.T) {
	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, "ReReply", cfg.App.Name)
	assert.Equal(t, 8080, cfg.Server.Port)
}

func TestLoad_MissingFileReturnsError(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.toml")
	require.Error(t, err)
}

func TestLoad_RateLimitDefaults(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, ""))
	require.NoError(t, err)
	assert.Equal(t, 10, cfg.RateLimit.LoginMaxAttempts)
	assert.Equal(t, 10, cfg.RateLimit.RegisterMaxAttempts)
	assert.Equal(t, 30, cfg.RateLimit.RefreshMaxAttempts)
	assert.Equal(t, 10, cfg.RateLimit.SSOMaxAttempts)
}

func TestResolveCredentials_StaticWhenNoSecret(t *testing.T) {
	s := config.ICEServerConfig{Username: "user", Credential: "pass"}
	username, credential := s.ResolveCredentials(time.Now())
	assert.Equal(t, "user", username)
	assert.Equal(t, "pass", credential)
}

func TestResolveCredentials_GeneratesRESTCredentials(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := config.ICEServerConfig{Secret: "topsecret", CredentialTTL: 3600}

	username, credential := s.ResolveCredentials(now)

	// Username is the expiry unix timestamp (now + ttl).
	require.Equal(t, "1003600", username)

	// Credential is base64(HMAC-SHA1(secret, username)) — computed independently.
	mac := hmac.New(sha1.New, []byte("topsecret"))
	mac.Write([]byte(username))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	assert.Equal(t, want, credential)
}

func TestResolveCredentials_SecretTakesPriorityAndPrefixesUsername(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := config.ICEServerConfig{Username: "alice", Credential: "static", Secret: "topsecret", CredentialTTL: 3600}

	username, credential := s.ResolveCredentials(now)

	require.Equal(t, "1003600:alice", username)
	assert.NotEqual(t, "static", credential)
}

func TestResolveCredentials_DefaultsTTLWhenUnset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := config.ICEServerConfig{Secret: "topsecret"}
	username, _ := s.ResolveCredentials(now)
	assert.Equal(t, "1086400", username) // now + default 86400s
}

// TestLoad_EnvMapsMultiWordKeys is the regression for the embedded-signup bug
// (whatomate#476): env vars for keys whose section OR field name contains an
// underscore must map correctly. Levels are separated by "__"; single
// underscores stay part of the key. This exercises config.Load()'s env path,
// which the handler-level tests bypass by setting the struct directly.
func TestLoad_EnvMapsMultiWordKeys(t *testing.T) {
	t.Setenv("WHATOMATE_WHATSAPP__APP_ID", "env-app-id")
	t.Setenv("WHATOMATE_WHATSAPP__CONFIG_ID", "env-config-id")
	t.Setenv("WHATOMATE_WHATSAPP__API_VERSION", "v21.0")
	t.Setenv("WHATOMATE_DEFAULT_ADMIN__EMAIL", "admin@example.com")
	t.Setenv("WHATOMATE_DATABASE__HOST", "db.internal")

	cfg, err := config.Load("") // no file; env-only
	require.NoError(t, err)

	assert.Equal(t, "env-app-id", cfg.WhatsApp.AppID)
	assert.Equal(t, "env-config-id", cfg.WhatsApp.ConfigID)
	assert.Equal(t, "v21.0", cfg.WhatsApp.APIVersion)
	assert.Equal(t, "admin@example.com", cfg.DefaultAdmin.Email)
	assert.Equal(t, "db.internal", cfg.Database.Host)
}

func TestLoad_EnvMapsS3CompatibleStorage(t *testing.T) {
	t.Setenv("WHATOMATE_STORAGE__TYPE", "s3")
	t.Setenv("WHATOMATE_STORAGE__S3_BUCKET", "rereply-media")
	t.Setenv("WHATOMATE_STORAGE__S3_REGION", "sin")
	t.Setenv("WHATOMATE_STORAGE__S3_ENDPOINT", "https://storage.railway.app")
	t.Setenv("WHATOMATE_STORAGE__S3_USE_PATH_STYLE", "true")

	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.Equal(t, "s3", cfg.Storage.Type)
	assert.Equal(t, "rereply-media", cfg.Storage.S3Bucket)
	assert.Equal(t, "sin", cfg.Storage.S3Region)
	assert.Equal(t, "https://storage.railway.app", cfg.Storage.S3Endpoint)
	assert.True(t, cfg.Storage.S3UsePathStyle)
}

func TestLoad_EnvMapsMetaRelayTrustConfig(t *testing.T) {
	const expectedAccounts = `{"accounts":[{"rereply_account_id":"00000000-0000-4000-8000-000000000001"}]}`
	const proofSecret = "deployment-meta-provider-proof-secret-at-least-32-bytes"
	t.Setenv("WHATOMATE_META_RELAY__BASE_URL", "https://relay.example.test/meta")
	t.Setenv("WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON", expectedAccounts)
	t.Setenv("WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET", proofSecret)

	cfg, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, "https://relay.example.test/meta", cfg.MetaRelay.BaseURL)
	assert.Equal(t, expectedAccounts, cfg.MetaRelay.ExpectedAccountsJSON)
	assert.Equal(t, proofSecret, cfg.MetaRelay.ProviderProofSecret)
}

func TestLoad_MetaRelayTrustConfigFailsClosedWhenPartial(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		base  string
		proof string
		want  string
	}{
		{
			name:  "missing base URL",
			proof: "deployment-meta-provider-proof-secret-at-least-32-bytes",
			want:  "BASE_URL",
		},
		{
			name: "missing provider proof",
			base: "https://relay.example.test/meta",
			want: "PROVIDER_PROOF_SECRET",
		},
		{
			name:  "short provider proof",
			base:  "https://relay.example.test/meta",
			proof: "too-short",
			want:  "at least 32 bytes",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("WHATOMATE_META_RELAY__BASE_URL", testCase.base)
			t.Setenv("WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON", `{"accounts":[]}`)
			t.Setenv("WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET", testCase.proof)
			_, err := config.Load("")
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestLoad_EnvMapsManagedMetaMessengerOnboarding(t *testing.T) {
	setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", "100000000000001")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__CONFIG_ID", "1720929458946813")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__OWNER_BUSINESS_ID", "2018290039073161")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", "server-only-meta-app-secret")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_API_VERSION", "v25.0")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_BASE_URL", "https://graph.facebook.com")
	t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", "https://relay.example.test/meta")

	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.True(t, cfg.MetaMessengerOnboarding.Enabled)
	assert.Equal(t, "100000000000001", cfg.MetaMessengerOnboarding.AppID)
	assert.Equal(t, "1720929458946813", cfg.MetaMessengerOnboarding.ConfigID)
	assert.Equal(t, "2018290039073161", cfg.MetaMessengerOnboarding.OwnerBusinessID)
	assert.Equal(t, "server-only-meta-app-secret", cfg.MetaMessengerOnboarding.AppSecret)
	assert.Equal(t, "v25.0", cfg.MetaMessengerOnboarding.GraphAPIVersion)
	assert.Equal(t, "https://graph.facebook.com", cfg.MetaMessengerOnboarding.GraphBaseURL)
	assert.Equal(t, "https://relay.example.test/meta", cfg.MetaMessengerOnboarding.TrustedRelayBaseURL)
}

func TestLoad_ManagedMetaMessengerOnboardingFailsClosedWhenEnabledButPartial(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		appID         string
		appSecret     string
		trustedRelay  string
		wantErrorText string
	}{
		{
			name:          "missing app ID",
			appSecret:     "server-only-meta-app-secret",
			trustedRelay:  "https://relay.example.test/meta",
			wantErrorText: "APP_ID",
		},
		{
			name:          "missing app secret",
			appID:         "100000000000001",
			trustedRelay:  "https://relay.example.test/meta",
			wantErrorText: "APP_SECRET",
		},
		{
			name:          "missing trusted relay",
			appID:         "100000000000001",
			appSecret:     "server-only-meta-app-secret",
			wantErrorText: "TRUSTED_RELAY_BASE_URL",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setMetaMessengerRelayTestEnv(t, "https://relay.example.test/meta")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__ENABLED", "true")
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID", testCase.appID)
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET", testCase.appSecret)
			t.Setenv("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL", testCase.trustedRelay)

			_, err := config.Load("")
			require.ErrorContains(t, err, testCase.wantErrorText)
		})
	}
}
