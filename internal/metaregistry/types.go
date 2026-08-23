// Package metaregistry defines the private control-plane contract used by the
// Meta relay. It deliberately contains no database or HTTP-framework code so
// both the ReReply API and the isolated relay can share the same validation.
package metaregistry

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	SchemaVersion = 1
	ResolvePath   = "/internal/meta-registry/v1/resolve"
	RevokePath    = "/internal/meta-registry/v1/revoke"
	ReviewPath    = "/internal/meta-registry/v1/revalidation"

	OwnershipVerified = "verified"
	OwnershipStale    = "stale"
	OwnershipRevoked  = "revoked"

	ResolvePurposeInbound  = "inbound"
	ResolvePurposeHealth   = "health"
	ResolvePurposeOutbound = "outbound"
	ResolvePurposeWorker   = "worker"

	ManagementModePlatformOAuth = "platform_oauth"

	// MessengerBusinessAuthorityMetadataKey records the independently verified
	// Business authority used when Meta omits the literal business_management
	// label from a Business Integration System User debug-token response.
	MessengerBusinessAuthorityMetadataKey = "meta_business_authority"
	// MessengerBusinessAuthoritySystemUserExactEdges is written only after the
	// exact client_business_id, assigned_pages, accounts, and owned_pages edges
	// have all been revalidated for the selected Page.
	MessengerBusinessAuthoritySystemUserExactEdges    = "bisu_exact_asset_edges_v1"
	MessengerBusinessAuthorityCheckedAtMetadataKey    = "meta_business_authority_checked_at"
	MessengerBusinessAuthorityOAuthIDMetadataKey      = "meta_business_authority_oauth_credential_id"
	MessengerBusinessAuthorityOAuthVersionMetadataKey = "meta_business_authority_oauth_version"
	MessengerBusinessAuthorityAppIDMetadataKey        = "meta_business_authority_app_id"
	MessengerBusinessAuthorityUserIDMetadataKey       = "meta_business_authority_user_id"
	MessengerBusinessAuthorityBusinessIDMetadataKey   = "meta_business_authority_business_id"
	MessengerBusinessAuthorityPageIDMetadataKey       = "meta_business_authority_page_id"
)

var (
	ErrInvalidRequest = errors.New("invalid Meta registry request")
	ErrNotFound       = errors.New("meta registry binding not found")
	ErrUnavailable    = errors.New("meta registry unavailable")
	ErrStaleBinding   = errors.New("meta registry binding is stale")
)

// ResolveRequest contains only the globally routable Meta asset identity. The
// organization is intentionally absent: the control plane discovers its one
// owner and then enters that tenant's RLS transaction.
type ResolveRequest struct {
	Channel           models.Channel `json:"channel"`
	ExternalAccountID string         `json:"external_account_id"`
	Purpose           string         `json:"purpose"`
}

// Binding is a short-lived secret-bearing lease. It must never be logged,
// persisted by the relay, returned to a browser, or included in an audit row.
type Binding struct {
	SchemaVersion            int            `json:"schema_version"`
	LeaseID                  uuid.UUID      `json:"lease_id"`
	LeaseExpiresAt           time.Time      `json:"lease_expires_at"`
	OrganizationID           uuid.UUID      `json:"organization_id"`
	ChannelAccountID         uuid.UUID      `json:"channel_account_id"`
	Channel                  models.Channel `json:"channel"`
	ExternalAccountID        string         `json:"external_account_id"`
	PlatformAppID            string         `json:"platform_app_id,omitempty"`
	InstagramAPIMode         string         `json:"instagram_api_mode,omitempty"`
	ReReplyWebhookURL        string         `json:"rereply_webhook_url"`
	AccessToken              string         `json:"access_token"`
	InboundSecret            string         `json:"inbound_secret"`
	OutboundSecret           string         `json:"outbound_secret"`
	CredentialID             uuid.UUID      `json:"credential_id"`
	CredentialVersion        int            `json:"credential_version"`
	WebhookCredentialID      uuid.UUID      `json:"webhook_credential_id"`
	WebhookCredentialVersion int            `json:"webhook_credential_version"`
	OwnershipCheckedAt       time.Time      `json:"ownership_checked_at"`
}

type MutationRequest struct {
	ChannelAccountID         uuid.UUID `json:"channel_account_id"`
	CredentialID             uuid.UUID `json:"credential_id"`
	CredentialVersion        int       `json:"credential_version"`
	WebhookCredentialID      uuid.UUID `json:"webhook_credential_id"`
	WebhookCredentialVersion int       `json:"webhook_credential_version"`
	Outcome                  string    `json:"outcome,omitempty"`
	Reason                   string    `json:"reason,omitempty"`
	CheckedAt                time.Time `json:"checked_at,omitempty"`
}

type MutationResponse struct {
	Applied bool `json:"applied"`
}

func NormalizeResolveRequest(request ResolveRequest) (ResolveRequest, error) {
	request.Channel = models.Channel(strings.ToLower(strings.TrimSpace(string(request.Channel))))
	request.ExternalAccountID = strings.TrimSpace(request.ExternalAccountID)
	request.Purpose = strings.ToLower(strings.TrimSpace(request.Purpose))
	if request.Channel != models.ChannelMessenger && request.Channel != models.ChannelInstagram {
		return ResolveRequest{}, ErrInvalidRequest
	}
	if request.ExternalAccountID == "" || len(request.ExternalAccountID) > 255 {
		return ResolveRequest{}, ErrInvalidRequest
	}
	switch request.Purpose {
	case ResolvePurposeInbound, ResolvePurposeHealth, ResolvePurposeOutbound, ResolvePurposeWorker:
	default:
		return ResolveRequest{}, ErrInvalidRequest
	}
	return request, nil
}

func (binding Binding) Validate(now time.Time) error {
	if binding.SchemaVersion != SchemaVersion || binding.LeaseID == uuid.Nil ||
		binding.OrganizationID == uuid.Nil || binding.ChannelAccountID == uuid.Nil ||
		binding.CredentialID == uuid.Nil || binding.CredentialVersion < 1 ||
		binding.WebhookCredentialID == uuid.Nil || binding.WebhookCredentialVersion < 1 {
		return ErrInvalidRequest
	}
	request, err := NormalizeResolveRequest(ResolveRequest{
		Channel: binding.Channel, ExternalAccountID: binding.ExternalAccountID,
		Purpose: ResolvePurposeInbound,
	})
	if err != nil || request.Channel != binding.Channel ||
		strings.TrimSpace(binding.ReReplyWebhookURL) == "" ||
		strings.TrimSpace(binding.AccessToken) == "" ||
		strings.TrimSpace(binding.InboundSecret) == "" ||
		strings.TrimSpace(binding.OutboundSecret) == "" ||
		binding.OwnershipCheckedAt.IsZero() {
		return ErrInvalidRequest
	}
	switch binding.Channel {
	case models.ChannelMessenger:
		if strings.TrimSpace(binding.PlatformAppID) == "" {
			return ErrInvalidRequest
		}
	case models.ChannelInstagram:
		mode := strings.TrimSpace(binding.InstagramAPIMode)
		if (mode != "instagram_login" && mode != "facebook_login") ||
			strings.TrimSpace(binding.PlatformAppID) == "" {
			return ErrInvalidRequest
		}
	}
	if binding.Channel != models.ChannelInstagram && strings.TrimSpace(binding.InstagramAPIMode) != "" {
		return ErrInvalidRequest
	}
	if !binding.LeaseExpiresAt.After(now.UTC()) {
		return ErrStaleBinding
	}
	return nil
}
