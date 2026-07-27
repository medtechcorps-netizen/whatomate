package channel

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

// ValidateOutboundPurpose deliberately restricts the first omnichannel release
// to service and transactional traffic. Marketing requires a separate campaign
// policy engine for quiet hours, frequency caps, and jurisdictional consent.
func ValidateOutboundPurpose(purpose models.ChannelPreferencePurpose) error {
	switch purpose {
	case models.ChannelPreferencePurposeService,
		models.ChannelPreferencePurposeTransactional:
		return nil
	case models.ChannelPreferencePurposeMarketing:
		return errors.New("marketing messages are not supported by the omnichannel inbox")
	case "":
		return errors.New("message purpose is required")
	default:
		return fmt.Errorf("message purpose %q is not supported", purpose)
	}
}

// OutboundConsentAllowed checks durable negative consent signals immediately
// before queueing and again immediately before provider delivery. Missing state
// is allowed only because ValidateOutboundPurpose excludes marketing.
func OutboundConsentAllowed(
	db *gorm.DB,
	organizationID, contactID, channelAccountID uuid.UUID,
	channelName models.Channel,
	purpose models.ChannelPreferencePurpose,
	now time.Time,
) (bool, string, error) {
	if err := ValidateOutboundPurpose(purpose); err != nil {
		return false, "unsupported_purpose", nil
	}
	if db == nil || organizationID == uuid.Nil || contactID == uuid.Nil || channelAccountID == uuid.Nil {
		return false, "invalid_consent_scope", errors.New("complete tenant consent scope is required")
	}
	now = now.UTC()

	var preference models.ContactChannelPreference
	err := db.Where(
		"organization_id = ? AND contact_id = ? AND channel_account_id = ? AND purpose = ?",
		organizationID,
		contactID,
		channelAccountID,
		purpose,
	).First(&preference).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, "preference_lookup_failed", err
	}
	if err == nil {
		switch preference.Status {
		case models.ChannelPreferenceStatusOptedOut:
			return false, "channel_preference_opted_out", nil
		case models.ChannelPreferenceStatusBlocked:
			return false, "channel_preference_blocked", nil
		}
	}

	var consent models.ConsentState
	err = db.Where(
		"organization_id = ? AND contact_id = ? AND purpose = ? AND channel = ?",
		organizationID,
		contactID,
		purpose,
		channelName,
	).Order("effective_at DESC, created_at DESC").First(&consent).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, "no_negative_consent_signal", nil
	}
	if err != nil {
		return false, "consent_lookup_failed", err
	}
	if consent.ExpiresAt != nil && !consent.ExpiresAt.After(now) {
		return false, "consent_expired", nil
	}
	switch consent.Status {
	case models.ConsentStatusDenied:
		return false, "consent_denied", nil
	case models.ConsentStatusWithdrawn:
		return false, "consent_withdrawn", nil
	case models.ConsentStatusExpired:
		return false, "consent_expired", nil
	default:
		return true, "consent_permits", nil
	}
}

// ValidateServiceWindow requires an approved business-initiation template when
// a provider-enforced customer service window is closed.
func ValidateServiceWindow(
	capabilities Capabilities,
	serviceWindowEndsAt *time.Time,
	parts []MessagePart,
	now time.Time,
) error {
	if !capabilities.ServiceWindow ||
		(serviceWindowEndsAt != nil && serviceWindowEndsAt.After(now.UTC())) {
		return nil
	}
	hasTemplate := false
	for _, part := range parts {
		if part.Type == models.MessagePartTypeTemplate {
			hasTemplate = true
			break
		}
	}
	if !hasTemplate || !capabilities.Templates || !capabilities.BusinessInitiation {
		return errors.New("the provider service window is closed; an approved initiation template is required")
	}
	return nil
}
