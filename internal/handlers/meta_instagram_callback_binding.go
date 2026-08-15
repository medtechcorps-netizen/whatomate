package handlers

import (
	"strings"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
)

const metaInstagramOAuthSubjectIDKey = "meta_oauth_subject_id"

// exactManagedInstagramCallbackBinding closes the callback lookup/mutation
// race. Instagram Login signed callbacks identify the app-scoped OAuth
// subject. Webhook/subscription routing independently uses the professional
// Instagram account ID. Callback handlers must run this check before changing
// any tenant state and must never conflate those two provider identities.
func exactManagedInstagramCallbackBinding(
	account *models.ChannelAccount,
	expectedAppID, signedUserID string,
) bool {
	expectedAppID = strings.TrimSpace(expectedAppID)
	signedUserID = strings.TrimSpace(signedUserID)
	return account != nil &&
		validCanonicalMetaID(expectedAppID) &&
		validCanonicalMetaID(signedUserID) &&
		account.Channel == models.ChannelInstagram &&
		account.Provider == channelapi.RelayProvider &&
		boolConfigValue(account.Config, "meta_registry_managed") &&
		stringConfigValue(account.Config, "meta_management_mode") == metaregistry.ManagementModePlatformOAuth &&
		stringConfigValue(account.Config, "instagram_api_mode") == "instagram_login" &&
		stringConfigValue(account.Metadata, "meta_webhook_app") == "instagram_login" &&
		stringConfigValue(account.Metadata, "meta_platform_app_id") == expectedAppID &&
		validCanonicalMetaID(account.ExternalAccountID) &&
		stringConfigValue(account.Metadata, "meta_authority_asset_id") == account.ExternalAccountID &&
		stringConfigValue(account.Metadata, "meta_authorizing_user_id") == signedUserID &&
		stringConfigValue(account.Metadata, metaInstagramOAuthSubjectIDKey) == signedUserID
}
