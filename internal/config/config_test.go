package config_test

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // matches the coturn TURN REST API derivation under test
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	assert.Equal(t, 30, cfg.MetaRegistry.LeaseSeconds)
	assert.Equal(t, 1440, cfg.MetaRegistry.OwnershipMaxAgeMins)
	assert.Equal(t, 300, cfg.MetaRegistry.ReplayWindowSeconds)
	assert.Equal(t, "local", cfg.Storage.Type)
	assert.Equal(t, "./uploads", cfg.Storage.LocalPath)
	assert.Equal(t, "admin@rereply.app", cfg.DefaultAdmin.Email)
	assert.Equal(t, "admin", cfg.DefaultAdmin.Password)
	assert.Equal(t, "ReReply Administrator", cfg.DefaultAdmin.FullName)
}

func TestLoad_RejectsWeakMetaRegistryServiceConfiguration(t *testing.T) {
	_, err := config.Load(writeConfig(t, `
[meta_registry]
service_secret = "too-short"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32 bytes")

	_, err = config.Load(writeConfig(t, `
[meta_registry]
lease_seconds = 301
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside safe bounds")

	_, err = config.Load(writeConfig(t, `
[meta_registry]
replay_window_seconds = 299
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside safe bounds")

	_, err = config.Load(writeConfig(t, `
[meta_registry]
service_secret = "synthetic-meta-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-meta-registry-edge-secret-at-least-32-bytes"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "explicitly enabled")
}

func TestLoad_AllowsCompleteDefaultOffMessengerLifecycleOnlyWithReaderFence(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[app]
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-meta-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-meta-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_messenger_onboarding]
enabled = true
app_id = "123456789012345"
config_id = "987654321098765"
app_secret = "synthetic-meta-app-secret-at-least-32-bytes"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
allowed_organization_ids = "11111111-1111-4111-8111-111111111111"
`))
	require.NoError(t, err)
	assert.True(t, cfg.MetaRegistry.Enabled)
	assert.True(t, cfg.MetaMessenger.Enabled)
	assert.Equal(t, 2, cfg.MetaRegistry.QueueReaderVersion)
}

func TestLoad_MessengerPilotReleaseRequiresUnambiguousCanonicalOrganizationGate(t *testing.T) {
	base := `
[app]
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-meta-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-meta-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_messenger_onboarding]
enabled = true
app_id = "123456789012345"
config_id = "987654321098765"
app_secret = "synthetic-meta-app-secret-at-least-32-bytes"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
`
	_, err := config.Load(writeConfig(t, base))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "NOT-A-UUID"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical UUIDs")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "11111111-1111-4111-8111-111111111111"
allow_all_organizations = true
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")

	_, err = config.Load(writeConfig(t, base+`
allow_all_organizations = true
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "production release")
}

func TestLoad_ProductionStaticConfigurationRemainsRegistryFree(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[app]
environment = "production"

[database]
host = "db.example.test"
`))
	require.NoError(t, err)
	assert.Empty(t, cfg.MetaRegistry.ServiceSecret)
	assert.Empty(t, cfg.MetaRegistry.RelayEdgeSecret)
	assert.Equal(t, 300, cfg.MetaRegistry.ReplayWindowSeconds)
}

func TestLoad_MessengerLifecycleRejectsServerDeadlineBelowProviderBudget(t *testing.T) {
	_, err := config.Load(writeConfig(t, `
[app]
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 119

[meta_registry]
enabled = true
service_secret = "synthetic-meta-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-meta-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_messenger_onboarding]
enabled = true
app_id = "123456789012345"
config_id = "987654321098765"
app_secret = "synthetic-meta-app-secret-at-least-32-bytes"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
allowed_organization_ids = "11111111-1111-4111-8111-111111111111"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write_timeout")
}

func TestLoad_ManagedInstagramProductionRequiresServerOwnedApprovalEvidence(t *testing.T) {
	base := `
[app]
environment = "production"
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_instagram_onboarding]
enabled = true
app_id = "111122223333444"
app_secret = "synthetic-instagram-secret-at-least-32-bytes"
rereply_base_url = "https://app.rereply.app"
relay_base_url = "https://app.rereply.app/meta-relay"
allowed_organization_id = "11111111-1111-4111-8111-111111111111"
`
	_, err := config.Load(writeConfig(t, base+`
app_review_status = "pending"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approved app review")

	quarantineCfg, err := config.Load(writeConfig(t, base+`
app_review_status = "rejected"
quarantine_only = true
`))
	require.NoError(t, err)
	assert.True(t, quarantineCfg.MetaInstagram.QuarantineOnly)

	_, err = config.Load(writeConfig(t, base+`
app_review_status = "rejected"
quarantine_only = true
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "tester"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden in production")

	_, err = config.Load(writeConfig(t, base+`
app_review_status = "approved"
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "tester"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden in production")

	cfg, err := config.Load(writeConfig(t, base+`
app_review_status = "approved"
`))
	require.NoError(t, err)
	assert.True(t, cfg.MetaInstagram.Enabled)
	assert.Equal(t, "approved", cfg.MetaInstagram.AppReviewStatus)
	assert.Empty(t, cfg.MetaInstagram.DevelopmentTestProfileID)

	for name, replacement := range map[string]string{
		"alternate host":        `rereply_base_url = "https://other.example.test"`,
		"deceptive host suffix": `rereply_base_url = "https://app.rereply.app.evil.example"`,
		"userinfo":              `rereply_base_url = "https://user@app.rereply.app"`,
		"query":                 `rereply_base_url = "https://app.rereply.app?next=elsewhere"`,
		"fragment":              `rereply_base_url = "https://app.rereply.app#elsewhere"`,
		"relay path suffix":     `relay_base_url = "https://app.rereply.app/meta-relay.evil"`,
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base + "\napp_review_status = \"approved\"\n"
			if strings.HasPrefix(replacement, "rereply_base_url") {
				candidate = strings.Replace(
					candidate, `rereply_base_url = "https://app.rereply.app"`, replacement, 1,
				)
			} else {
				candidate = strings.Replace(
					candidate, `relay_base_url = "https://app.rereply.app/meta-relay"`, replacement, 1,
				)
			}
			_, loadErr := config.Load(writeConfig(t, candidate))
			require.Error(t, loadErr)
		})
	}
}

func TestLoad_ManagedInstagramProviderOriginsRejectAdversarialURLs(t *testing.T) {
	base := `
[app]
environment = "production"
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_instagram_onboarding]
enabled = true
app_id = "111122223333444"
app_secret = "synthetic-instagram-secret-at-least-32-bytes"
app_review_status = "approved"
authorization_base_url = "https://www.instagram.com"
token_base_url = "https://api.instagram.com"
graph_base_url = "https://graph.instagram.com"
rereply_base_url = "https://app.rereply.app"
relay_base_url = "https://app.rereply.app/meta-relay"
allowed_organization_id = "11111111-1111-4111-8111-111111111111"
`
	_, err := config.Load(writeConfig(t, base))
	require.NoError(t, err)

	providers := []struct {
		name      string
		field     string
		origin    string
		errorText string
	}{
		{
			name: "authorization", field: "authorization_base_url",
			origin: "https://www.instagram.com", errorText: "authorization_base_url",
		},
		{
			name: "token", field: "token_base_url",
			origin: "https://api.instagram.com", errorText: "token_base_url",
		},
		{
			name: "graph", field: "graph_base_url",
			origin: "https://graph.instagram.com", errorText: "graph_base_url",
		},
	}
	attacks := []struct {
		name  string
		value func(string) string
	}{
		{name: "bare query", value: func(origin string) string { return origin + "?" }},
		{name: "query", value: func(origin string) string { return origin + "?next=elsewhere" }},
		{name: "path", value: func(origin string) string { return origin + "/oauth" }},
		{name: "root path", value: func(origin string) string { return origin + "/" }},
		{name: "host suffix", value: func(origin string) string { return origin + ".evil.example" }},
		{name: "userinfo", value: func(origin string) string {
			return strings.Replace(origin, "https://", "https://user@", 1)
		}},
		{name: "fragment", value: func(origin string) string { return origin + "#elsewhere" }},
		{name: "opaque", value: func(origin string) string {
			return strings.Replace(origin, "https://", "https:", 1)
		}},
	}

	for _, provider := range providers {
		for _, attack := range attacks {
			t.Run(provider.name+"/"+attack.name, func(t *testing.T) {
				validLine := provider.field + ` = "` + provider.origin + `"`
				invalidLine := provider.field + ` = "` + attack.value(provider.origin) + `"`
				candidate := strings.Replace(base, validLine, invalidLine, 1)
				_, loadErr := config.Load(writeConfig(t, candidate))
				require.Error(t, loadErr)
				assert.Contains(t, loadErr.Error(), provider.errorText)
			})
		}
	}
}

func TestLoad_ManagedInstagramDevelopmentBootstrapIsExactAndNonproductionOnly(t *testing.T) {
	base := `
[app]
environment = "development"
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_instagram_onboarding]
enabled = true
app_id = "111122223333444"
app_secret = "synthetic-instagram-secret-at-least-32-bytes"
app_review_status = "not_submitted"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
allowed_organization_id = "11111111-1111-4111-8111-111111111111"
`
	_, err := config.Load(writeConfig(t, base))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "development_test_profile_id")

	_, err = config.Load(writeConfig(t, base+`
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "workspace_admin"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact administrator, developer, or tester")

	cfg, err := config.Load(writeConfig(t, base+`
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "tester"
`))
	require.NoError(t, err)
	assert.Equal(t, "555566667777888", cfg.MetaInstagram.DevelopmentTestProfileID)
	assert.Equal(t, "666677778888999", cfg.MetaInstagram.DevelopmentTestOAuthSubjectID)
	assert.Equal(t, "tester", cfg.MetaInstagram.DevelopmentAppRole)

	for _, environment := range []string{"prod", "qa"} {
		t.Run("unknown environment "+environment, func(t *testing.T) {
			candidate := strings.Replace(
				base,
				`environment = "development"`,
				`environment = "`+environment+`"`,
				1,
			) + `
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "tester"
`
			_, loadErr := config.Load(writeConfig(t, candidate))
			require.Error(t, loadErr)
			assert.Contains(t, loadErr.Error(), "environment must be development, staging, or production")
		})
	}
}

func TestLoad_ManagedInstagramOrganizationSetsAreBoundedAndBackwardCompatible(t *testing.T) {
	const (
		activeA    = "11111111-1111-4111-8111-111111111111"
		activeB    = "22222222-2222-4222-8222-222222222222"
		quarantine = "33333333-3333-4333-8333-333333333333"
		compliance = "99999999-9999-4999-8999-999999999999"
	)
	base := `
[app]
environment = "production"
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_instagram_onboarding]
enabled = true
app_id = "111122223333444"
app_secret = "synthetic-instagram-secret-at-least-32-bytes"
app_review_status = "approved"
rereply_base_url = "https://app.rereply.app"
relay_base_url = "https://app.rereply.app/meta-relay"
`

	legacy, err := config.Load(writeConfig(t, base+`
allowed_organization_id = "`+activeA+`"
`))
	require.NoError(t, err)
	assert.False(t, legacy.MetaInstagram.UsesOrganizationSetModel())
	assert.Equal(t, []string{activeA}, legacy.MetaInstagram.ActiveOrganizationIDs())
	assert.Equal(t, []string{activeA}, legacy.MetaInstagram.ManagedOrganizationIDs())
	assert.Empty(t, legacy.MetaInstagram.DataDeletionComplianceOrganization())

	transition, err := config.Load(writeConfig(t, base+`
allowed_organization_id = "`+activeA+`"
allowed_organization_ids = "`+activeA+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.NoError(t, err)
	assert.True(t, transition.MetaInstagram.UsesOrganizationSetModel())
	assert.Equal(t, []string{activeA}, transition.MetaInstagram.ActiveOrganizationIDs())
	assert.Equal(t, compliance, transition.MetaInstagram.DataDeletionComplianceOrganization())

	multi, err := config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+activeB+`,`+activeA+`"
quarantined_organization_ids = "`+quarantine+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.NoError(t, err)
	assert.True(t, multi.MetaInstagram.UsesOrganizationSetModel())
	assert.Equal(t, []string{activeA, activeB}, multi.MetaInstagram.ActiveOrganizationIDs())
	assert.Equal(t, []string{activeA, activeB, quarantine}, multi.MetaInstagram.ManagedOrganizationIDs())
	assert.True(t, multi.MetaInstagram.OrganizationReleased(activeA))
	assert.False(t, multi.MetaInstagram.OrganizationReleased(quarantine))
	assert.True(t, multi.MetaInstagram.OrganizationManaged(quarantine))
	assert.True(t, multi.MetaInstagram.OrganizationQuarantined(quarantine))
	assert.Equal(t, compliance, multi.MetaInstagram.DataDeletionComplianceOrganization())
	multi.MetaInstagram.QuarantineOnly = true
	assert.False(t, multi.MetaInstagram.OrganizationReleased(activeA))
	assert.True(t, multi.MetaInstagram.OrganizationQuarantined(activeA))
	assert.True(t, multi.MetaInstagram.OrganizationQuarantined(quarantine))

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+activeA+`,`+activeB+`"
`))
	require.ErrorContains(t, err, "data_deletion_compliance_organization_id")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_id = "`+activeA+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.ErrorContains(t, err, "requires the organization-set model")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_id = "`+activeA+`"
allowed_organization_ids = "`+activeA+`,`+activeB+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.ErrorContains(t, err, "legacy allowed_organization_id")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+activeA+`"
quarantined_organization_ids = "`+activeA+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.ErrorContains(t, err, "must be disjoint")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+activeA+`"
data_deletion_compliance_organization_id = "`+activeA+`"
`))
	require.ErrorContains(t, err, "distinct from every managed clinic")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "00000000-0000-0000-0000-000000000000"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.ErrorContains(t, err, "allowed_organization_ids must contain canonical UUIDs")

	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+activeA+`"
data_deletion_compliance_organization_id = "00000000-0000-0000-0000-000000000000"
`))
	require.ErrorContains(t, err, "data_deletion_compliance_organization_id")

	tooMany := make([]string, 0, config.MaxMetaInstagramManagedOrganizations+1)
	for index := 1; index <= config.MaxMetaInstagramManagedOrganizations+1; index++ {
		tooMany = append(tooMany, fmt.Sprintf("00000000-0000-4000-8000-%012x", index))
	}
	_, err = config.Load(writeConfig(t, base+`
allowed_organization_ids = "`+strings.Join(tooMany, ",")+`"
data_deletion_compliance_organization_id = "`+compliance+`"
`))
	require.ErrorContains(t, err, "at most 32 managed organizations")
}

func TestLoad_ManagedInstagramAppMustRemainDistinctFromManagedMessenger(t *testing.T) {
	base := `
[app]
environment = "development"
encryption_key = "synthetic-encryption-key-at-least-32-bytes"

[server]
write_timeout = 120

[meta_registry]
enabled = true
service_secret = "synthetic-registry-service-secret-at-least-32-bytes"
relay_edge_secret = "synthetic-registry-edge-secret-at-least-32-bytes"
queue_reader_version = 2

[meta_messenger_onboarding]
enabled = true
app_id = "111122223333444"
config_id = "444433332222111"
app_secret = "synthetic-shared-app-secret-at-least-32-bytes"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
allowed_organization_ids = "11111111-1111-4111-8111-111111111111"
allow_development_user_token = true

[meta_instagram_onboarding]
enabled = true
app_id = "111122223333444"
app_secret = "synthetic-instagram-secret-at-least-32-bytes"
app_review_status = "not_submitted"
rereply_base_url = "https://app.example.test"
relay_base_url = "https://relay.example.test"
allowed_organization_id = "11111111-1111-4111-8111-111111111111"
development_test_profile_id = "555566667777888"
development_test_oauth_subject_id = "666677778888999"
development_app_role = "developer"
`
	_, err := config.Load(writeConfig(t, base))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "distinct IDs and secrets")

	_, err = config.Load(writeConfig(t, strings.Replace(
		base,
		"app_id = \"111122223333444\"\napp_secret = \"synthetic-instagram-secret-at-least-32-bytes\"",
		"app_id = \"999900001111222\"\napp_secret = \"synthetic-shared-app-secret-at-least-32-bytes\"",
		1,
	)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "distinct IDs and secrets")
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

func TestLoad_ThreadsDevelopmentTestingGateIsExactAndNonProductionOnly(t *testing.T) {
	t.Run("staging exact allowlist", func(t *testing.T) {
		cfg, err := config.Load(writeConfig(t, `
[app]
environment = "staging"

[threads_app_review]
development_testing_enabled = true
development_organization_id = "11111111-1111-4111-8111-111111111111"
development_app_id = "123456789012345"
development_profile_id = "987654321098765"
`))
		require.NoError(t, err)
		assert.True(t, cfg.ThreadsAppReview.DevelopmentTestingEnabled)
	})

	for _, testCase := range []struct {
		name    string
		content string
		message string
	}{
		{
			name: "production rejects even a complete gate",
			content: `
[app]
environment = "production"

[threads_app_review]
development_testing_enabled = true
development_organization_id = "11111111-1111-4111-8111-111111111111"
development_app_id = "123456789012345"
development_profile_id = "987654321098765"
`,
			message: "forbidden in production",
		},
		{
			name: "allowlist requires explicit enablement",
			content: `
[threads_app_review]
development_organization_id = "11111111-1111-4111-8111-111111111111"
development_app_id = "123456789012345"
development_profile_id = "987654321098765"
`,
			message: "explicitly enabled",
		},
		{
			name: "workspace must be canonical",
			content: `
[threads_app_review]
development_testing_enabled = true
development_organization_id = "11111111-1111-4111-8111-11111111111A"
development_app_id = "123456789012345"
development_profile_id = "987654321098765"
`,
			message: "canonical UUID",
		},
		{
			name: "profile must be numeric",
			content: `
[threads_app_review]
development_testing_enabled = true
development_organization_id = "11111111-1111-4111-8111-111111111111"
development_app_id = "123456789012345"
development_profile_id = "profile-1"
`,
			message: "canonical numeric Meta IDs",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, testCase.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.message)
		})
	}
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
