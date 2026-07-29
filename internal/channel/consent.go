package channel

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
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

	allowed := false
	reason := "consent_lookup_failed"
	err := canonicalConsentTransaction(db, func(tx *gorm.DB) error {
		canonicalContact, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
			tx,
			organizationID,
			contactID,
		)
		if resolveErr != nil {
			reason = "contact_lookup_failed"
			return resolveErr
		}
		canonicalContactID := canonicalContact.ID

		var preference models.ContactChannelPreference
		preferenceErr := tx.Where(
			"organization_id = ? AND contact_id = ? AND channel_account_id = ? AND purpose = ?",
			organizationID,
			canonicalContactID,
			channelAccountID,
			purpose,
		).First(&preference).Error
		if preferenceErr != nil && !errors.Is(preferenceErr, gorm.ErrRecordNotFound) {
			reason = "preference_lookup_failed"
			return preferenceErr
		}
		if preferenceErr == nil {
			switch preference.Status {
			case models.ChannelPreferenceStatusOptedOut:
				allowed = false
				reason = "channel_preference_opted_out"
				return nil
			case models.ChannelPreferenceStatusBlocked:
				allowed = false
				reason = "channel_preference_blocked"
				return nil
			}
		}

		var consents []models.ConsentState
		if consentErr := tx.Where(
			"organization_id = ? AND contact_id = ? AND purpose = ? AND channel = ?",
			organizationID,
			canonicalContactID,
			purpose,
			channelName,
		).Order("effective_at DESC, created_at DESC").Find(&consents).Error; consentErr != nil {
			reason = "consent_lookup_failed"
			return consentErr
		}
		if len(consents) == 0 {
			allowed = true
			reason = "no_negative_consent_signal"
			return nil
		}

		// ConsentState is unique per subject key, so every row here is the
		// current state for one identity of the canonical contact. A newer
		// grant for one identity must never override a denial or withdrawal for
		// another.
		for _, consent := range consents {
			if consent.Status == models.ConsentStatusDenied {
				allowed = false
				reason = "consent_denied"
				return nil
			}
		}
		for _, consent := range consents {
			if consent.Status == models.ConsentStatusWithdrawn {
				allowed = false
				reason = "consent_withdrawn"
				return nil
			}
		}
		for _, consent := range consents {
			if consent.Status == models.ConsentStatusExpired ||
				(consent.ExpiresAt != nil && !consent.ExpiresAt.After(now)) {
				allowed = false
				reason = "consent_expired"
				return nil
			}
		}
		allowed = true
		reason = "consent_permits"
		return nil
	})
	if err != nil {
		return false, reason, err
	}
	return allowed, reason, nil
}

const canonicalConsentAttempts = 3

func canonicalConsentTransaction(db *gorm.DB, check func(*gorm.DB) error) error {
	var err error
	for attempt := 0; attempt < canonicalConsentAttempts; attempt++ {
		err = db.Transaction(check)
		if !canonicalConsentRetryable(err) {
			return err
		}
	}
	return err
}

func canonicalConsentRetryable(err error) bool {
	if errors.Is(err, contactutil.ErrCanonicalContactChanged) {
		return true
	}
	var sqlState interface {
		SQLState() string
	}
	if !errors.As(err, &sqlState) {
		return false
	}
	switch sqlState.SQLState() {
	case "40001", "40P01":
		return true
	default:
		return false
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
