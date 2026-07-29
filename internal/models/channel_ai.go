package models

import (
	"time"

	"github.com/google/uuid"
)

const (
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
