package channel_test

import (
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// Fixture setup obeys the same fence precondition as account-only admission.
func ensureFencedLegacyMetaAccountForTest(db *gorm.DB, ref channelapi.LegacyMetaAccountRef) (*models.ChannelAccount, error) {
	var shadow *models.ChannelAccount
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := database.LockOrganizationPolicyScope(tx, ref.OrganizationID); err != nil {
			return err
		}
		var err error
		shadow, err = channelapi.EnsureLegacyMetaWhatsAppAccount(tx, ref)
		return err
	})
	return shadow, err
}
