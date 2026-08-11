package metatrust

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

func TestManagedMessengerTrustRequiresServerOwnedEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mutate    func(*models.ChannelAccount)
		wantError string
	}{
		{
			name: "matching owned pages and app evidence",
		},
		{
			name: "inventory cannot substitute another page business",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_business_id"] = "999999999999999"
			},
			wantError: "ownership evidence",
		},
		{
			name: "missing owned pages evidence version",
			mutate: func(account *models.ChannelAccount) {
				delete(account.Metadata, "ownership_evidence_version")
			},
			wantError: "ownership evidence",
		},
		{
			name: "invalid owned pages evidence timestamp",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["ownership_verified_at"] = "not-a-timestamp"
			},
			wantError: "timestamp is invalid",
		},
		{
			name: "managed metadata app id mismatch",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_app_id"] = "999999999999991"
			},
			wantError: "app evidence",
		},
		{
			name: "managed metadata app owner mismatch",
			mutate: func(account *models.ChannelAccount) {
				account.Metadata["meta_app_owner_business_id"] = "999999999999992"
			},
			wantError: "app evidence",
		},
		{
			name: "tenant config cannot substitute missing managed app metadata",
			mutate: func(account *models.ChannelAccount) {
				delete(account.Metadata, "meta_app_id")
				account.Config["meta_app_id"] = trustMessengerAppID
				account.Config["meta_app_owner_business_id"] = trustMessengerAppOwner
			},
			wantError: "app evidence",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			account := trustedManagedMessengerAccount()
			if testCase.mutate != nil {
				testCase.mutate(account)
			}
			_, err := Resolve(trustedTestSettings(), "production", account)
			if testCase.wantError == "" {
				if err != nil {
					t.Fatalf("expected managed Messenger trust to resolve: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("Resolve error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func TestManagedMessengerTrustPinsProtectedMessengerAppInventory(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mutate    func(map[string]any)
		wantError string
	}{
		{
			name: "protected app id mismatch",
			mutate: func(document map[string]any) {
				messengerApp(document)["app_id"] = "999999999999993"
			},
			wantError: "app evidence",
		},
		{
			name: "protected app owner mismatch",
			mutate: func(document map[string]any) {
				messengerApp(document)["owner_business_id"] = "999999999999994"
			},
			wantError: "app evidence",
		},
		{
			name: "missing protected messenger app",
			mutate: func(document map[string]any) {
				delete(document, "messenger_app")
			},
			wantError: "app binding is missing or invalid",
		},
		{
			name: "missing protected app owner",
			mutate: func(document map[string]any) {
				delete(messengerApp(document), "owner_business_id")
			},
			wantError: "app binding is missing or invalid",
		},
		{
			name: "invalid protected app id",
			mutate: func(document map[string]any) {
				messengerApp(document)["app_id"] = "not-a-meta-id"
			},
			wantError: "messenger_app.app_id",
		},
		{
			name: "non-string protected app id",
			mutate: func(document map[string]any) {
				messengerApp(document)["app_id"] = 100000000000001
			},
			wantError: "inventory is invalid",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settings := trustedTestSettings()
			mutateTrustedInventory(t, &settings, testCase.mutate)
			_, err := Resolve(settings, "production", trustedManagedMessengerAccount())
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("Resolve error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func TestManagedMessengerTrustDoesNotUseConfigOrConfigIDAsTrust(t *testing.T) {
	account := trustedManagedMessengerAccount()
	account.Config["meta_app_id"] = "999999999999995"
	account.Config["meta_app_owner_business_id"] = "999999999999996"
	account.Config["meta_config_id"] = "999999999999997"
	account.Metadata["meta_config_id"] = "audit-fingerprint-not-a-trust-anchor"

	if _, err := Resolve(trustedTestSettings(), "production", account); err != nil {
		t.Fatalf("tenant Config and audit-only config ID must not affect trust: %v", err)
	}
}

func TestLegacyMessengerTrustDoesNotRequireProtectedMessengerApp(t *testing.T) {
	settings := trustedTestSettings()
	mutateTrustedInventory(t, &settings, func(document map[string]any) {
		delete(document, "messenger_app")
	})

	if _, err := Resolve(settings, "production", trustedTestAccount()); err != nil {
		t.Fatalf("legacy inventory-governed Messenger account should remain supported: %v", err)
	}
}

func trustedManagedMessengerAccount() *models.ChannelAccount {
	account := trustedTestAccount()
	account.Metadata = models.JSONB{
		"management_mode":            "meta_messenger_oauth",
		"meta_business_id":           trustBusinessID,
		"meta_app_id":                trustMessengerAppID,
		"meta_app_owner_business_id": trustMessengerAppOwner,
		"ownership_evidence_version": "owned_pages_v1",
		"ownership_verified_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	return account
}

func mutateTrustedInventory(t *testing.T, settings *config.MetaRelayConfig, mutate func(map[string]any)) {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(settings.ExpectedAccountsJSON), &document); err != nil {
		t.Fatalf("decode trusted test inventory: %v", err)
	}
	mutate(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode trusted test inventory: %v", err)
	}
	settings.ExpectedAccountsJSON = string(encoded)
}

func messengerApp(document map[string]any) map[string]any {
	app, _ := document["messenger_app"].(map[string]any)
	return app
}
