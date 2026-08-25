package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateWhatsAppConfigPinsProductionGraphOrigin(t *testing.T) {
	require.NoError(t, validateWhatsAppConfig(
		WhatsAppConfig{BaseURL: "https://graph.facebook.com"},
		"production",
	))

	for _, candidate := range []string{
		"http://graph.facebook.com",
		"https://graph.facebook.com/",
		"https://GRAPH.facebook.com",
		"https://graph.facebook.com?token=secret",
		"https://graph.example.test",
		"http://pilot-whatsapp-graph-sink:8083",
		" https://graph.facebook.com ",
	} {
		t.Run(candidate, func(t *testing.T) {
			require.Error(t, validateWhatsAppConfig(
				WhatsAppConfig{BaseURL: candidate},
				"production",
			))
		})
	}
}

func TestValidateWhatsAppConfigAllowsStagingTestOrigin(t *testing.T) {
	require.NoError(t, validateWhatsAppConfig(
		WhatsAppConfig{
			BaseURL:    "http://staging-whatsapp-graph-sink:8083",
			APIVersion: "v21.0",
		},
		"staging",
	))
}

func TestValidateWhatsAppConfigRejectsNonOriginValues(t *testing.T) {
	for _, candidate := range []string{
		"",
		"graph.facebook.com",
		"ftp://graph.facebook.com",
		"https://user:password@graph.facebook.com",
		"https://graph.facebook.com/v24.0",
		"https://graph.facebook.com/#fragment",
	} {
		t.Run(candidate, func(t *testing.T) {
			require.Error(t, validateWhatsAppConfig(
				WhatsAppConfig{BaseURL: candidate},
				"staging",
			))
		})
	}
}
