package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCloudflareTURNConfigIsOptionalValidatedAndSecretIsNotSerialized(t *testing.T) {
	key := strings.Repeat("a", 32)
	token := "synthetic-cloudflare-api-token"
	for _, value := range []config.CloudflareTURNConfig{{}, {KeyID: key, APIToken: token}} {
		require.NoError(t, value.Validate())
	}
	for _, value := range []config.CloudflareTURNConfig{{KeyID: key}, {APIToken: token}, {KeyID: "../other", APIToken: token}, {KeyID: key, APIToken: token + "\nInjected: value"}} {
		err := value.Validate()
		require.Error(t, err)
		require.NotContains(t, err.Error(), token)
	}
	cfg := config.Config{Calling: config.CallingConfig{CloudflareTURN: config.CloudflareTURNConfig{KeyID: key, APIToken: token}}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(raw), token)
	require.NotContains(t, string(raw), "APIToken")
}

func TestLoadCloudflareTURNRuntimeEnvironmentAndConflictingStaticMode(t *testing.T) {
	t.Setenv("WHATOMATE_CALLING__CLOUDFLARE_TURN__KEY_ID", strings.Repeat("a", 32))
	t.Setenv("WHATOMATE_CALLING__CLOUDFLARE_TURN__API_TOKEN", "synthetic-cloudflare-api-token")
	cfg, err := config.Load(writeConfig(t, ""))
	require.NoError(t, err)
	require.True(t, cfg.Calling.CloudflareTURN.Configured())
	_, err = config.Load(writeConfig(t, "[[calling.ice_servers]]\nurls = ['stun:example.invalid:3478']\n"))
	require.Error(t, err)
	require.NotContains(t, err.Error(), cfg.Calling.CloudflareTURN.APIToken)
}
