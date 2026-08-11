package channel

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderCredentialRequirementAllowsSeparateOAuthCredential(t *testing.T) {
	t.Parallel()

	kind, ok := ProviderRequiredCredentialKind(models.ChannelMessenger, RelayProvider)
	require.True(t, ok)
	assert.Equal(t, models.ChannelCredentialKindWebhook, kind)

	now := time.Now().UTC()
	credentials := []models.ChannelCredential{
		{Kind: models.ChannelCredentialKindWebhook, Status: models.ChannelCredentialStatusActive},
		{Kind: models.ChannelCredentialKindOAuth, Status: models.ChannelCredentialStatusActive},
	}
	assert.Equal(t, 1, CurrentCredentialCountOfKind(credentials, kind, now))

	credentials = append(credentials, models.ChannelCredential{
		Kind:   models.ChannelCredentialKindWebhook,
		Status: models.ChannelCredentialStatusExpiring,
	})
	assert.Equal(t, 2, CurrentCredentialCountOfKind(credentials, kind, now))
}

func TestRelayCredentialSelectionUsesNewestVersionDeterministically(t *testing.T) {
	t.Parallel()

	oldSecret, err := appcrypto.Encrypt("old-secret", relayTestEncryptionKey)
	require.NoError(t, err)
	newSecret, err := appcrypto.Encrypt("new-secret", relayTestEncryptionKey)
	require.NoError(t, err)

	account := relayTestAccount(t, "https://relay.example.test/events")
	account.Credentials = []models.ChannelCredential{
		{
			BaseModel: models.BaseModel{ID: uuid.MustParse(
				"ffffffff-ffff-4fff-8fff-ffffffffffff",
			)},
			Kind:    models.ChannelCredentialKindWebhook,
			Version: 1,
			Status:  models.ChannelCredentialStatusActive,
			CredentialBlob: models.JSONB{
				"inbound_secret": oldSecret,
			},
		},
		{
			BaseModel: models.BaseModel{ID: uuid.MustParse(
				"00000000-0000-4000-8000-000000000001",
			)},
			Kind:    models.ChannelCredentialKindWebhook,
			Version: 2,
			Status:  models.ChannelCredentialStatusActive,
			CredentialBlob: models.JSONB{
				"inbound_secret": newSecret,
			},
		},
	}

	body := []byte(`{"external_account_id":"page-123","events":[]}`)
	newHeaders := http.Header{}
	newHeaders.Set(RelaySignatureHeader, signRelayBody("new-secret", body))
	newHeaders.Set(
		RelayMetaProviderProofHeader,
		SignMetaProviderInboundProof(relayTestProviderProofSecret, body),
	)
	oldHeaders := http.Header{}
	oldHeaders.Set(RelaySignatureHeader, signRelayBody("old-secret", body))
	oldHeaders.Set(
		RelayMetaProviderProofHeader,
		SignMetaProviderInboundProof(relayTestProviderProofSecret, body),
	)
	adapter := NewRelayAdapter(
		models.ChannelInstagram,
		nil,
		relayTestEncryptionKey,
	).
		WithExpectedMetaBusinessID(relayTestMetaBusinessID).
		WithMetaProviderProofSecret(relayTestProviderProofSecret)

	require.NoError(t, adapter.VerifyWebhook(account, newHeaders, body))
	assert.ErrorIs(
		t,
		adapter.VerifyWebhook(account, oldHeaders, body),
		ErrRelaySignatureInvalid,
	)

	account.Credentials[0], account.Credentials[1] =
		account.Credentials[1], account.Credentials[0]
	require.NoError(t, adapter.VerifyWebhook(account, newHeaders, body))
	assert.ErrorIs(
		t,
		adapter.VerifyWebhook(account, oldHeaders, body),
		ErrRelaySignatureInvalid,
	)
}

func TestRelayCredentialSelectionIgnoresOAuthSecrets(t *testing.T) {
	t.Parallel()

	rogueSecret, err := appcrypto.Encrypt("oauth-must-not-sign-relay", relayTestEncryptionKey)
	require.NoError(t, err)
	account := relayTestAccount(t, "https://relay.example.test/events")
	account.Credentials = append(account.Credentials, models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Kind:      models.ChannelCredentialKindOAuth,
		Version:   99,
		Status:    models.ChannelCredentialStatusActive,
		CredentialBlob: models.JSONB{
			"inbound_secret":  rogueSecret,
			"outbound_secret": rogueSecret,
		},
	})

	body := []byte(`{"external_account_id":"page-123","events":[]}`)
	headers := http.Header{}
	headers.Set(RelaySignatureHeader, signRelayBody("inbound-secret", body))
	headers.Set(
		RelayMetaProviderProofHeader,
		SignMetaProviderInboundProof(relayTestProviderProofSecret, body),
	)
	adapter := NewRelayAdapter(
		models.ChannelInstagram,
		nil,
		relayTestEncryptionKey,
	).
		WithExpectedMetaBusinessID(relayTestMetaBusinessID).
		WithMetaProviderProofSecret(relayTestProviderProofSecret)

	require.NoError(t, adapter.VerifyWebhook(account, headers, body))
	headers.Set(RelaySignatureHeader, signRelayBody("oauth-must-not-sign-relay", body))
	assert.ErrorIs(t, adapter.VerifyWebhook(account, headers, body), ErrRelaySignatureInvalid)
}
