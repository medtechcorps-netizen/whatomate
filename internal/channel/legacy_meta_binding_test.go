package channel_test

import (
	"testing"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyMetaWhatsAppAccountIDFailsClosedOnInconsistentBinding(t *testing.T) {
	legacyID := uuid.New()
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    uuid.New(),
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.LegacyMetaProvider,
		ExternalAccountID: "legacy-account:" + legacyID.String(),
		Metadata:          models.JSONB{"legacy_account_id": legacyID.String()},
	}

	got, err := channelapi.LegacyMetaWhatsAppAccountID(account)
	require.NoError(t, err)
	assert.Equal(t, legacyID, got)

	account.Metadata["legacy_account_id"] = uuid.NewString()
	_, err = channelapi.LegacyMetaWhatsAppAccountID(account)
	require.ErrorContains(t, err, "inconsistent")

	account.Metadata = models.JSONB{}
	_, err = channelapi.LegacyMetaWhatsAppAccountID(account)
	require.ErrorContains(t, err, "missing")
}
