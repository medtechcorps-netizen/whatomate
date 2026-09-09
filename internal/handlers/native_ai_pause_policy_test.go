package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIncomingAutomaticAISuppressionIsStickyMetadata(t *testing.T) {
	message := &models.Message{
		Direction: models.DirectionIncoming,
		Metadata: models.JSONB{
			incomingAutomaticAISuppressedKey:        true,
			incomingAutomaticAISuppressionReasonKey: "identity_review_hold",
			incomingAutomaticAISuppressedAtKey:      "2026-09-06T00:00:00Z",
		},
	}
	assert.True(t, incomingMessageAutomaticAISuppressed(message))

	// Later policy state is intentionally not consulted here. Once the exact
	// WAMID is admitted as suppressed, Resume can affect only later messages.
	message.Metadata["current_conversation_ai_state"] = "active"
	assert.True(t, incomingMessageAutomaticAISuppressed(message))

	message.Direction = models.DirectionOutgoing
	assert.False(t, incomingMessageAutomaticAISuppressed(message))
}

func TestInboundActionIdentityUsesDurableScopeNotPayload(t *testing.T) {
	messageID := uuid.New()
	newExecution := func() *App {
		return &App{inboundContinuation: &inboundContinuationExecution{
			OrganizationID: uuid.New(),
			ContactID:      uuid.New(),
			MessageID:      messageID,
			WAMID:          "wamid.stable-action",
			actionScope:    "chat-graph:flow:node:visit:7",
		}}
	}

	first, err := newExecution().nextInboundContinuationActionKey(
		"graph_api_call",
		map[string]any{"secret": "first"},
	)
	require.NoError(t, err)
	second, err := newExecution().nextInboundContinuationActionKey(
		"graph_api_call",
		map[string]any{"secret": "changed"},
	)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.NotContains(t, first.Key, "secret")
}

func TestChatGraphVisitOrdinalRequiresDurableCanonicalPath(t *testing.T) {
	session := &models.ChatbotSession{SessionData: models.JSONB{}}
	ordinal, err := chatGraphVisitOrdinal(session)
	require.NoError(t, err)
	assert.Zero(t, ordinal)

	session.SessionData["__path__"] = []any{
		map[string]any{"node": "first"},
		map[string]any{"node": "second"},
	}
	ordinal, err = chatGraphVisitOrdinal(session)
	require.NoError(t, err)
	assert.Equal(t, 2, ordinal)

	session.SessionData["__path__"] = "caller-selected-ordinal"
	_, err = chatGraphVisitOrdinal(session)
	require.Error(t, err)
}

func TestChatGraphPublicMappingCannotOverwriteReservedIdentity(t *testing.T) {
	destination := models.JSONB{"__path__": []any{"durable"}}
	copyChatGraphPublicSessionData(destination, map[string]any{
		"customer_tier": "gold",
		"__path__":      []any{"forged"},
		" __private":    "forged",
	})
	assert.Equal(t, "gold", destination["customer_tier"])
	assert.Equal(t, []any{"durable"}, destination["__path__"])
	assert.NotContains(t, destination, " __private")
}
