package worker

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/queue"
)

const realtimeWorkerPublishTimeout = 2 * time.Second

// publishChannelMessageStatus runs only after a worker settlement transaction
// commits. Realtime delivery is best-effort and must never replay a provider
// send or rewrite its durable result.
func (w *Worker) publishChannelMessageStatus(
	organizationID, channelAccountID, conversationID uuid.UUID,
	messageID *uuid.UUID,
	status string,
) {
	if w == nil || w.Publisher == nil || organizationID == uuid.Nil {
		return
	}
	event := &queue.RealtimeEvent{
		OrganizationID: organizationID,
		SourceID:       "worker",
		Kind:           queue.RealtimeEventMessageStatusChanged,
		MessageID:      messageID,
		Status:         status,
		OccurredAt:     time.Now().UTC(),
	}
	if channelAccountID != uuid.Nil {
		accountID := channelAccountID
		event.ChannelAccountID = &accountID
	}
	if conversationID != uuid.Nil {
		conversation := conversationID
		event.ConversationID = &conversation
	}
	ctx, cancel := context.WithTimeout(context.Background(), realtimeWorkerPublishTimeout)
	err := w.Publisher.PublishRealtime(ctx, event)
	cancel()
	if err != nil {
		w.Log.Warn(
			"Failed to publish channel message status invalidation",
			"error", err,
			"organization_id", organizationID,
			"message_id", messageID,
			"status", status,
		)
	}
}
