package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReaformPilotConfigurationFailsClosed(t *testing.T) {
	valid := ReaformPilotConfig{Enabled: true, OrganizationID: "10000000-0000-4000-8000-000000000001", RemoteOrganizationID: "10000000-0000-4000-8000-000000000002", RemoteSiteID: "10000000-0000-4000-8000-000000000003", APIBaseURL: "https://clinic.example.test", APIToken: strings.Repeat("a", 64)}
	require.NoError(t, ValidateReaformPilotConfig(ReaformPilotConfig{}))
	require.NoError(t, ValidateReaformPilotConfig(valid))
	for _, base := range []string{"http://clinic.example.test", "https://localhost", "https://127.0.0.1", "https://10.0.0.2", "https://[::1]", "https://user:pass@clinic.example.test", "https://clinic.example.test/path", "https://clinic.example.test?token=x", "https://clinic.example.test:444"} {
		t.Run(base, func(t *testing.T) { c := valid; c.APIBaseURL = base; require.Error(t, ValidateReaformPilotConfig(c)) })
	}
	c := valid
	c.OrganizationID = ""
	require.Error(t, ValidateReaformPilotConfig(c))
	c = valid
	c.RemoteSiteID = "10000000-0000-4000-8000-00000000000A"
	require.Error(t, ValidateReaformPilotConfig(c))
	c = valid
	c.APIToken = strings.Repeat("A", 64)
	require.Error(t, ValidateReaformPilotConfig(c))
}

func TestLoadReaformPilotIsolatedAndValidated(t *testing.T) {
	for _, key := range []string{"ENABLED", "ORGANIZATION_ID", "REMOTE_ORGANIZATION_ID", "REMOTE_SITE_ID", "API_BASE_URL", "API_TOKEN", "TERMINAL_RETENTION_DAYS"} {
		t.Setenv("WHATOMATE_REAFORM_PILOT__"+key, "")
	}
	t.Setenv("WHATOMATE_REAFORM_PILOT__ENABLED", "false")
	cfg, err := LoadReaformPilot("")
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	t.Setenv("WHATOMATE_REAFORM_PILOT__ENABLED", "true")
	_, err = LoadReaformPilot("")
	require.Error(t, err)
	t.Setenv("WHATOMATE_REAFORM_PILOT__ORGANIZATION_ID", "10000000-0000-4000-8000-000000000001")
	t.Setenv("WHATOMATE_REAFORM_PILOT__REMOTE_ORGANIZATION_ID", "10000000-0000-4000-8000-000000000002")
	t.Setenv("WHATOMATE_REAFORM_PILOT__REMOTE_SITE_ID", "10000000-0000-4000-8000-000000000003")
	t.Setenv("WHATOMATE_REAFORM_PILOT__API_BASE_URL", "https://clinic.example.test")
	t.Setenv("WHATOMATE_REAFORM_PILOT__API_TOKEN", strings.Repeat("a", 64))
	t.Setenv("WHATOMATE_REAFORM_PILOT__TERMINAL_RETENTION_DAYS", "120")
	file := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(file, []byte("[reaform_pilot]\nenabled = false\nterminal_retention_days = 5\n"), 0600))
	cfg, err = LoadReaformPilot(file)
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	require.Equal(t, 120, cfg.TerminalRetentionDays)
	require.Equal(t, "https://clinic.example.test", cfg.APIBaseURL)
	t.Setenv("WHATOMATE_REAFORM_PILOT__ENABLED", "not-a-boolean")
	_, err = LoadReaformPilot(file)
	require.Error(t, err)
}
