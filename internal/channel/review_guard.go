package channel

import (
	"strings"

	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
)

const metaMessengerOAuthManagementMode = "meta_messenger_oauth"

// IsStagingMessengerReviewMarked is a denial-only safety predicate. A marker
// is never sufficient to grant inbound trust; handlers must also match the
// deployment-owned review tuple. It is sufficient to deny Test, egress, AI,
// and credential-control operations, where a false positive fails safely.
func IsStagingMessengerReviewMarked(account *models.ChannelAccount) bool {
	if account == nil ||
		account.Channel != models.ChannelMessenger ||
		!strings.EqualFold(strings.TrimSpace(account.Provider), RelayProvider) ||
		configStringValue(account.Metadata, "management_mode") != metaMessengerOAuthManagementMode ||
		configStringValue(account.Metadata, "review_relay_mode") != metareview.Marker {
		return false
	}
	return true
}

func configStringValue(values models.JSONB, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
