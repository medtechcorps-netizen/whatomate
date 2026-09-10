package channel_test

import (
	"testing"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestEnsureLegacyMetaWhatsAppAccountRejectsCorruptBindingWithoutRefresh(t *testing.T) {
	for _, name := range []string{"missing", "conflicting"} {
		t.Run(name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := createLegacyMetaTestOrganization(t, db, "binding-"+name)
			account := createLegacyMetaTestAccount(t, db, organization.ID, "Expected-"+uuid.NewString())
			shadow, err := channelapi.EnsureLegacyMetaWhatsAppAccount(db, legacyMetaRef(account))
			require.NoError(t, err)
			require.NotNil(t, shadow)

			metadata := models.JSONB{"legacy_account_name": account.Name}
			if name == "conflicting" {
				other := createLegacyMetaTestAccount(t, db, organization.ID, "Other-"+uuid.NewString())
				metadata["legacy_account_id"] = other.ID.String()
			}
			require.NoError(t, db.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", shadow.ID, organization.ID).
				Update("metadata", metadata).Error)
			var before models.ChannelAccount
			require.NoError(t, db.First(&before, "id = ?", shadow.ID).Error)

			err = db.Transaction(func(tx *gorm.DB) error {
				refreshed, refreshErr := channelapi.EnsureLegacyMetaWhatsAppAccount(tx, legacyMetaRef(account))
				assert.Nil(t, refreshed)
				var during models.ChannelAccount
				if err := tx.First(&during, "id = ?", shadow.ID).Error; err != nil {
					return err
				}
				assert.Equal(t, before, during, "binding validation must precede refresh, not rely on rollback")
				return refreshErr
			})
			require.ErrorIs(t, err, channelapi.ErrLegacyMetaBridgeConflict)
			var after models.ChannelAccount
			require.NoError(t, db.First(&after, "id = ?", shadow.ID).Error)
			assert.Equal(t, before, after, "a rejected refresh must preserve the complete existing shadow")
		})
	}
}
