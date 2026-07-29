package channel

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	oldHeaders := http.Header{}
	oldHeaders.Set(RelaySignatureHeader, signRelayBody("old-secret", body))
	adapter := NewRelayAdapter(
		models.ChannelInstagram,
		nil,
		relayTestEncryptionKey,
	)

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
