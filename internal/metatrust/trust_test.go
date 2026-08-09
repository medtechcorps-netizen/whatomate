package metatrust

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	trustAccountID         = "00000000-0000-4000-8000-000000000001"
	trustOrgID             = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
	trustBusinessID        = "300000000000099"
	trustExternalID        = "700000000000099"
	trustMessengerAppID    = "100000000000001"
	trustMessengerAppOwner = "300000000000001"
)

func TestResolvePinsProtectedInventoryBusinessAndExactPrefixedURL(t *testing.T) {
	account := trustedTestAccount()
	settings := trustedTestSettings()
	binding, err := Resolve(settings, "production", account)
	if err != nil {
		t.Fatalf("resolve exact trust binding: %v", err)
	}
	if binding.MetaBusinessID != trustBusinessID ||
		binding.RelayURL != "https://app.rereply.app/meta-relay/v1/accounts/messenger/"+trustExternalID {
		t.Fatalf("unexpected binding: %#v", binding)
	}
}

func TestResolveRejectsTenantURLAndInventoryMismatches(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*models.ChannelAccount, *config.MetaRelayConfig)
		want   string
	}{
		{
			name: "arbitrary relay origin",
			mutate: func(account *models.ChannelAccount, _ *config.MetaRelayConfig) {
				account.Config["relay_url"] = "https://attacker.example/v1/accounts/messenger/" + trustExternalID
			},
			want: "trusted Meta relay endpoint",
		},
		{
			name: "arbitrary health URL",
			mutate: func(account *models.ChannelAccount, _ *config.MetaRelayConfig) {
				account.Config["health_url"] = "https://attacker.example/healthy"
			},
			want: "health_url",
		},
		{
			name: "wrong organization",
			mutate: func(account *models.ChannelAccount, _ *config.MetaRelayConfig) {
				account.OrganizationID = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2")
			},
			want: "absent or mismatched",
		},
		{
			name: "inventory omits account",
			mutate: func(_ *models.ChannelAccount, settings *config.MetaRelayConfig) {
				settings.ExpectedAccountsJSON = strings.ReplaceAll(settings.ExpectedAccountsJSON, trustAccountID, "00000000-0000-4000-8000-000000000002")
			},
			want: "absent or mismatched",
		},
		{
			name: "production HTTP base",
			mutate: func(account *models.ChannelAccount, settings *config.MetaRelayConfig) {
				settings.BaseURL = "http://127.0.0.1:8081/meta-relay"
				account.Config["relay_url"] = "http://127.0.0.1:8081/meta-relay/v1/accounts/messenger/" + trustExternalID
			},
			want: "must use HTTPS",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			account := trustedTestAccount()
			settings := trustedTestSettings()
			testCase.mutate(account, &settings)
			if _, err := Resolve(settings, "production", account); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Resolve error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestResolveRejectsAmbiguousProtectedInventory(t *testing.T) {
	settings := trustedTestSettings()
	duplicate := fmt.Sprintf(`,
      {
        "organization_id": %q,
        "meta_business_id": "999999999999999",
        "channel": "instagram",
        "external_account_id": "17841400000000000",
        "rereply_account_id": "00000000-0000-4000-8000-000000000002"
      }`, trustOrgID)
	settings.ExpectedAccountsJSON = strings.Replace(settings.ExpectedAccountsJSON, "\n  ]", duplicate+"\n  ]", 1)
	if _, err := Resolve(settings, "production", trustedTestAccount()); err == nil ||
		!strings.Contains(err.Error(), "multiple Meta Business IDs") {
		t.Fatalf("expected ambiguous ownership rejection, got %v", err)
	}
}

func TestValidateOutboundRequiresExactlyOneRelayWebhookAndAllowsOAuth(t *testing.T) {
	newReadyAccount := func() *models.ChannelAccount {
		account := trustedTestAccount()
		checkedAt := time.Now().UTC().Add(-time.Minute)
		inboundAt := checkedAt.Add(time.Second)
		account.Status = models.ChannelAccountStatusActive
		account.Config["identity_confirmed_id"] = account.ExternalAccountID
		account.Config["outbound_enabled"] = true
		account.Metadata = models.JSONB{
			"management_mode":                       "meta_messenger_oauth",
			"meta_business_id":                      trustBusinessID,
			"ownership_evidence_version":            "owned_pages_v1",
			"ownership_verified_at":                 checkedAt.Format(time.RFC3339Nano),
			"meta_app_id":                           trustMessengerAppID,
			"meta_app_owner_business_id":            trustMessengerAppOwner,
			channelapi.MetaProviderProofMetadataKey: channelapi.MetaProviderProofVersion,
		}
		account.LastHealthCheckAt = &checkedAt
		account.LastInboundAt = &inboundAt
		account.Credentials = []models.ChannelCredential{
			{
				Kind:   models.ChannelCredentialKindWebhook,
				Status: models.ChannelCredentialStatusActive,
			},
			{
				Kind:   models.ChannelCredentialKindOAuth,
				Status: models.ChannelCredentialStatusActive,
			},
		}
		return account
	}

	t.Run("one webhook plus OAuth is valid", func(t *testing.T) {
		_, err := ValidateOutbound(
			trustedTestSettings(),
			"production",
			newReadyAccount(),
			time.Now().UTC(),
		)
		if err != nil {
			t.Fatalf("validate managed Messenger credentials: %v", err)
		}
	})

	t.Run("OAuth cannot replace relay webhook", func(t *testing.T) {
		account := newReadyAccount()
		account.Credentials = account.Credentials[1:]
		_, err := ValidateOutbound(
			trustedTestSettings(),
			"production",
			account,
			time.Now().UTC(),
		)
		if err == nil || !strings.Contains(err.Error(), "exactly one current relay webhook") {
			t.Fatalf("expected missing webhook rejection, got %v", err)
		}
	})

	t.Run("duplicate current relay webhooks are ambiguous", func(t *testing.T) {
		account := newReadyAccount()
		account.Credentials = append(account.Credentials, models.ChannelCredential{
			Kind:   models.ChannelCredentialKindWebhook,
			Status: models.ChannelCredentialStatusExpiring,
		})
		_, err := ValidateOutbound(
			trustedTestSettings(),
			"production",
			account,
			time.Now().UTC(),
		)
		if err == nil || !strings.Contains(err.Error(), "exactly one current relay webhook") {
			t.Fatalf("expected duplicate webhook rejection, got %v", err)
		}
	})
}

func trustedTestAccount() *models.ChannelAccount {
	return &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.MustParse(trustAccountID)},
		OrganizationID:    uuid.MustParse(trustOrgID),
		Channel:           models.ChannelMessenger,
		Provider:          "relay",
		ExternalAccountID: trustExternalID,
		Config: models.JSONB{
			"relay_url": "https://app.rereply.app/meta-relay/v1/accounts/messenger/" + trustExternalID,
		},
	}
}

func trustedTestSettings() config.MetaRelayConfig {
	return config.MetaRelayConfig{
		BaseURL:             "https://app.rereply.app/meta-relay",
		ProviderProofSecret: "trusted-meta-provider-proof-secret-at-least-32-bytes",
		ExpectedAccountsJSON: fmt.Sprintf(`{
  "messenger_app": {
    "app_id": %q,
    "owner_business_id": %q
  },
  "instagram_app": {},
  "accounts": [
    {
      "key": "clinic-messenger",
      "organization_id": %q,
      "organization_name": "Clinic",
      "meta_business_id": %q,
      "channel": "messenger",
      "external_account_id": %q,
      "rereply_account_id": %q,
      "access_token_env": "PAGE_TOKEN",
      "rereply_inbound_secret_env": "PAGE_INBOUND",
      "rereply_outbound_secret_env": "PAGE_OUTBOUND"
    }
  ]
}`, trustMessengerAppID, trustMessengerAppOwner, trustOrgID, trustBusinessID, trustExternalID, trustAccountID),
	}
}
