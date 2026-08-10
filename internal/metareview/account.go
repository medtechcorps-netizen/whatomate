package metareview

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const reviewManagementMode = "meta_messenger_oauth"

// DeploymentIdentityMatches identifies the one deployment-pinned record for
// denial decisions. It deliberately does not rely on the review marker or
// other mutable metadata: deleting or corrupting those fields must never turn
// an inbound-only review account into an egress-capable account.
func DeploymentIdentityMatches(tuple ProvisionTuple, account *models.ChannelAccount) bool {
	if account == nil || tuple.validateStructure() != nil {
		return false
	}
	return account.ID != uuid.Nil &&
		account.ID.String() == tuple.ChannelAccountID &&
		account.OrganizationID != uuid.Nil &&
		account.OrganizationID.String() == tuple.OrganizationID &&
		account.Channel == models.ChannelMessenger &&
		strings.EqualFold(strings.TrimSpace(account.Provider), "relay") &&
		account.ExternalAccountID == tuple.PageID
}

// ReadyInboundOnlyBindingMatches is the positive trust predicate used before
// provisioning or accepting review traffic. Unlike the denial predicate it
// requires every server-owned marker and every egress fuse to be exact.
func ReadyInboundOnlyBindingMatches(tuple ProvisionTuple, account *models.ChannelAccount) bool {
	if !DeploymentIdentityMatches(tuple, account) ||
		tuple.Validate(time.Now().UTC()) != nil ||
		account.DeletedAt.Valid ||
		account.Provider != "relay" ||
		account.Status != models.ChannelAccountStatusPending ||
		account.ConnectedAt != nil ||
		account.IsDefaultOutgoing {
		return false
	}
	if exactString(account.Metadata, "management_mode") != reviewManagementMode ||
		exactString(account.Metadata, "review_relay_mode") != Marker ||
		exactString(account.Metadata, "review_generation") != tuple.Generation ||
		exactString(account.Metadata, "review_expires_at") != tuple.ExpiresAt ||
		exactString(account.Metadata, "meta_business_id") != tuple.MetaBusinessID ||
		exactString(account.Metadata, "meta_app_id") != tuple.MetaAppID ||
		!exactTrue(account.Metadata, "review_ready") ||
		!exactTrue(account.Metadata, "subscription_verified") {
		return false
	}
	return exactFalse(account.Config, "outbound_enabled") &&
		exactFalse(account.Config, "ai_reply_enabled")
}

func exactString(values models.JSONB, key string) string {
	value, _ := values[key].(string)
	return value
}

func exactTrue(values models.JSONB, key string) bool {
	value, ok := values[key].(bool)
	return ok && value
}

func exactFalse(values models.JSONB, key string) bool {
	value, ok := values[key].(bool)
	return ok && !value
}
