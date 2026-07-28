package channel

import (
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
)

// MetaCustomerServiceWindow is enforced by the server for customer
// conversations on Meta messaging channels. Tenant-supplied capability flags
// may add approved features, but they cannot disable this provider policy.
const MetaCustomerServiceWindow = 24 * time.Hour

// ApplyMandatoryProviderCapabilities applies provider rules that are not
// tenant-configurable. In particular, Instagram and Messenger always have a
// customer-service window even when an account attempts to set
// service_window=false.
func ApplyMandatoryProviderCapabilities(channel models.Channel, capabilities Capabilities) Capabilities {
	switch channel {
	case models.ChannelInstagram, models.ChannelMessenger:
		capabilities.ServiceWindow = true
	}
	return capabilities
}

// InboundServiceWindowAnchor returns the earliest trustworthy time associated
// with a verified inbound customer message. The server receipt is the upper
// bound, so a delayed relay delivery can never reopen Meta's 24-hour window.
// Provider times are usable only after the signed adapter has validated and
// future-capped them.
func InboundServiceWindowAnchor(
	serverAcceptedAt time.Time,
	verifiedProviderTimes ...time.Time,
) time.Time {
	anchor := serverAcceptedAt.UTC()
	for _, candidate := range verifiedProviderTimes {
		if candidate.IsZero() {
			continue
		}
		candidate = candidate.UTC()
		if anchor.IsZero() || candidate.Before(anchor) {
			anchor = candidate
		}
	}
	return anchor
}

// InboundServiceWindowEndsAt returns the provider-enforced window opened by a
// verified customer message at the already-derived policy anchor.
func InboundServiceWindowEndsAt(channel models.Channel, windowOpenedAt time.Time) *time.Time {
	switch channel {
	case models.ChannelInstagram, models.ChannelMessenger:
		if windowOpenedAt.IsZero() {
			return nil
		}
		end := windowOpenedAt.UTC().Add(MetaCustomerServiceWindow)
		return &end
	default:
		return nil
	}
}
