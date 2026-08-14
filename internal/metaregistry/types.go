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

	ManagementModePlatformOAuth = "platform_oauth"
)

var (
	ErrInvalidRequest = errors.New("invalid Meta registry request")
	ErrNotFound       = errors.New("Meta registry binding not found")
	ErrUnavailable    = errors.New("Meta registry unavailable")
	ErrStaleBinding   = errors.New("Meta registry binding is stale")
)

// ResolveRequest contains only the globally routable Meta asset identity. The
// organization is intentionally absent: the control plane discovers its one
// owner and then enters that tenant's RLS transaction.
type ResolveRequest struct {
	Channel           models.Channel `json:"channel"`
	ExternalAccountID string         `json:"external_account_id"`
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
	if request.Channel != models.ChannelMessenger && request.Channel != models.ChannelInstagram {
		return ResolveRequest{}, ErrInvalidRequest
	}
	if request.ExternalAccountID == "" || len(request.ExternalAccountID) > 255 {
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
	})
	if err != nil || request.Channel != binding.Channel ||
		strings.TrimSpace(binding.ReReplyWebhookURL) == "" ||
		strings.TrimSpace(binding.AccessToken) == "" ||
		strings.TrimSpace(binding.InboundSecret) == "" ||
		strings.TrimSpace(binding.OutboundSecret) == "" ||
		binding.OwnershipCheckedAt.IsZero() {
		return ErrInvalidRequest
	}
	if binding.Channel == models.ChannelInstagram {
		mode := strings.TrimSpace(binding.InstagramAPIMode)
		if mode != "instagram_login" && mode != "facebook_login" {
			return ErrInvalidRequest
		}
	} else if strings.TrimSpace(binding.InstagramAPIMode) != "" {
		return ErrInvalidRequest
	}
	if !binding.LeaseExpiresAt.After(now.UTC()) {
		return ErrStaleBinding
	}
	return nil
}
