package config

import (
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
