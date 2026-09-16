package models

import (
	"time"

	"github.com/google/uuid"
)

const (
	ChannelConfigAIBookingEnabled      = "ai_booking_enabled"
	ChannelConfigAIBookingRevision     = "ai_booking_revision"
	ChannelConfigAIBookingEnabledAt    = "ai_booking_enabled_at"
	ChannelConfigAIBookingRouteBinding = "ai_booking_route_sha256"

	ScheduledJobKindChannelAIReply = "channel_ai_reply"

	ChannelAIReplyAggregateType = "message"
	ChannelAIReplyKeyPrefix     = "channel-ai-reply:"

	ConversationConfigAIPaused          = "ai_paused"
	ConversationConfigAIPauseReason     = "ai_pause_reason"
	ConversationConfigAIPausedAt        = "ai_paused_at"
	ConversationConfigAIPausedByUserID  = "ai_paused_by_user_id"
	ConversationConfigAIResumedAt       = "ai_resumed_at"
	ConversationConfigAIResumedByUserID = "ai_resumed_by_user_id"
)

// ChannelAIReplyJobPayload binds one scheduled generation attempt to the
// canonical tenant/account/conversation/message tuple. Workers must re-read
// and revalidate every ID rather than trusting this payload.
type ChannelAIReplyJobPayload struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	ChannelAccountID uuid.UUID `json:"channel_account_id"`
	ConversationID   uuid.UUID `json:"conversation_id"`
	InboundMessageID uuid.UUID `json:"inbound_message_id"`
	ServiceWindowAt  time.Time `json:"service_window_at"`
}

func ChannelAIReplyIdempotencyKey(inboundMessageID uuid.UUID) string {
	return ChannelAIReplyKeyPrefix + inboundMessageID.String()
}

// AIBookingAuthority reads only an explicit per-channel grant. Global chatbot
// settings, truthy strings and the frequently refreshed UpdatedAt are not authority.
// Native WhatsApp callers must additionally verify the current shadow route.
func AIBookingAuthority(account *ChannelAccount) (string, bool) {
	if account == nil || account.ID == uuid.Nil || account.OrganizationID == uuid.Nil ||
		account.DeletedAt.Valid || account.Status != ChannelAccountStatusActive {
		return "", false
	}
	enabled, ok := account.Config[ChannelConfigAIBookingEnabled].(bool)
	if !ok || !enabled {
		return "", false
	}
	revision, ok := account.Config[ChannelConfigAIBookingRevision].(string)
	if !ok {
		return "", false
	}
	id, err := uuid.Parse(revision)
	if err != nil || id == uuid.Nil || id.String() != revision {
		return "", false
	}
	rawEpoch, ok := account.Config[ChannelConfigAIBookingEnabledAt].(string)
	if !ok {
		return "", false
	}
	epoch, err := time.Parse(time.RFC3339Nano, rawEpoch)
	if err != nil || epoch.IsZero() || epoch.UTC().Format(time.RFC3339Nano) != rawEpoch {
		return "", false
	}
	return revision, true
}

// AIBookingAuthorityForInbound prevents an OFF -> ON transition from granting
// booking authority to a message that was already queued before this opt-in.
func AIBookingAuthorityForInbound(account *ChannelAccount, inboundAt time.Time) (string, bool) {
	revision, ok := AIBookingAuthority(account)
	if !ok || inboundAt.IsZero() {
		return "", false
	}
	epoch, _ := time.Parse(time.RFC3339Nano, account.Config[ChannelConfigAIBookingEnabledAt].(string))
	if inboundAt.Before(epoch) {
		return "", false
	}
	return revision, true
}
