package channel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayAdapterIgnoresOutgoingMessageEcho(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	account := relayTestAccount(t, "https://relay.example.test/events")
	body := relayMessageBody(t, InboundMessage{
		ExternalMessageID: "echo-1",
		Conversation:      ConversationRef{ExternalID: "thread-1"},
		Sender: Participant{
			ExternalID: "page-123",
			Role:       models.ConversationParticipantRoleBot,
		},
		Direction: models.DirectionOutgoing,
		Parts: []MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "This was sent by ReReply",
		}},
	})

	events, err := adapter.NormalizeWebhook(context.Background(), account, body)

	require.NoError(t, err)
	assert.Empty(t, events, "outgoing echoes must not become canonical inbound events")
}

func TestRelayAdapterRequiresIncomingCustomerMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		direction models.Direction
		role      models.ConversationParticipantRole
		errorText string
	}{
		{
			name:      "missing direction",
			role:      models.ConversationParticipantRoleCustomer,
			errorText: "direction is required",
		},
		{
			name:      "unknown direction",
			direction: models.Direction("sideways"),
			role:      models.ConversationParticipantRoleCustomer,
			errorText: "direction \"sideways\" is not supported",
		},
		{
			name:      "missing sender role",
			direction: models.DirectionIncoming,
			errorText: "sender role must be customer",
		},
		{
			name:      "agent sender",
			direction: models.DirectionIncoming,
			role:      models.ConversationParticipantRoleAgent,
			errorText: "sender role must be customer",
		},
	}

	for _, fixture := range tests {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			adapter := NewRelayAdapter(models.ChannelMessenger, nil, relayTestEncryptionKey)
			account := relayTestAccount(t, "https://relay.example.test/events")
			account.Channel = models.ChannelMessenger
			body := relayMessageBody(t, InboundMessage{
				ExternalMessageID: "message-1",
				Conversation:      ConversationRef{ExternalID: "thread-1"},
				Sender: Participant{
					ExternalID: "customer-1",
					Role:       fixture.role,
				},
				Direction: fixture.direction,
				Parts: []MessagePart{{
					Type: models.MessagePartTypeText,
					Text: "Hello",
				}},
			})

			_, err := adapter.NormalizeWebhook(context.Background(), account, body)

			require.Error(t, err)
			assert.Contains(t, err.Error(), fixture.errorText)
		})
	}
}

func TestRelayAdapterCapsFutureProviderTimestamps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
	future := now.Add(365 * 24 * time.Hour)
	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	adapter.now = func() time.Time { return now }
	account := relayTestAccount(t, "https://relay.example.test/events")
	envelope := relayWebhookEnvelope{
		ExternalAccountID: account.ExternalAccountID,
		Events: []InboundEvent{{
			DedupeKey:  "future-event",
			Type:       NormalizedEventTypeMessage,
			OccurredAt: future,
			Message: &InboundMessage{
				ExternalMessageID: "message-future",
				Conversation:      ConversationRef{ExternalID: "thread-future"},
				Sender: Participant{
					ExternalID: "customer-future",
					Role:       models.ConversationParticipantRoleCustomer,
				},
				Direction:  models.DirectionIncoming,
				SentAt:     future,
				ReceivedAt: future,
				Parts: []MessagePart{{
					Type: models.MessagePartTypeText,
					Text: "Hello from the future",
				}},
			},
		}},
	}
	body, err := json.Marshal(envelope)
	require.NoError(t, err)

	events, err := adapter.NormalizeWebhook(context.Background(), account, body)

	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].Message)
	assert.Equal(t, now, events[0].OccurredAt)
	assert.Equal(t, now, events[0].Message.SentAt)
	assert.Equal(t, now, events[0].Message.ReceivedAt)
}

func TestRelayAdapterEnforcesTextAndContextBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		message   InboundMessage
		errorText string
	}{
		{
			name: "text too long",
			message: validRelayInboundMessage(MessagePart{
				Type: models.MessagePartTypeText,
				Text: strings.Repeat("x", MaxMessageTextRunes+1),
			}),
			errorText: "text is too long",
		},
		{
			name: "reply context too long",
			message: func() InboundMessage {
				message := validRelayInboundMessage(MessagePart{
					Type: models.MessagePartTypeText,
					Text: "Hello",
				})
				message.ReplyToExternalID = strings.Repeat("r", maxRelayReplyContextRunes+1)
				return message
			}(),
			errorText: "reply context is too long",
		},
		{
			name: "metadata context too large",
			message: func() InboundMessage {
				message := validRelayInboundMessage(MessagePart{
					Type: models.MessagePartTypeText,
					Text: "Hello",
				})
				message.Metadata = map[string]any{
					"untrusted": strings.Repeat("m", maxRelayMessageContextSize),
				}
				return message
			}(),
			errorText: "message context is too large",
		},
	}

	for _, fixture := range tests {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
			account := relayTestAccount(t, "https://relay.example.test/events")

			_, err := adapter.NormalizeWebhook(
				context.Background(),
				account,
				relayMessageBody(t, fixture.message),
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), fixture.errorText)
		})
	}
}

func TestMetaRelayCapabilitiesAreTextOnlyAndWindowCannotBeDisabled(t *testing.T) {
	t.Parallel()

	for _, channelName := range []models.Channel{
		models.ChannelInstagram,
		models.ChannelMessenger,
	} {
		channelName := channelName
		t.Run(string(channelName), func(t *testing.T) {
			t.Parallel()
			adapter := NewRelayAdapter(channelName, nil, relayTestEncryptionKey)
			account := relayTestAccount(t, "https://relay.example.test/events")
			account.Channel = channelName

			defaults := adapter.Capabilities(account)
			assert.True(t, defaults.Text)
			assert.True(t, defaults.ServiceWindow)
			assert.False(t, defaults.Media)
			assert.False(t, defaults.MultipleAttachments)
			assert.False(t, defaults.Buttons)
			assert.False(t, defaults.Templates)
			assert.False(t, defaults.ReadReceipts)
			assert.False(t, defaults.Typing)

			account.Capabilities = models.JSONB{
				"service_window": false,
				"media":          true,
			}
			configured := adapter.Capabilities(account)
			assert.True(t, configured.ServiceWindow, "tenant config cannot bypass Meta's service window")
			assert.True(t, configured.Media, "explicitly reviewed optional capabilities remain configurable")
		})
	}
}

func TestEmailRelayCapabilitiesMatchTextReplyTransport(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelEmail, nil, relayTestEncryptionKey)
	account := relayTestAccount(t, "https://relay.example.test/events")
	account.Channel = models.ChannelEmail

	capabilities := adapter.Capabilities(account)
	assert.True(t, capabilities.Text)
	assert.True(t, capabilities.Replies)
	assert.False(t, capabilities.Media)
	assert.False(t, capabilities.MultipleAttachments)
	assert.False(t, capabilities.ReadReceipts)
	assert.False(t, capabilities.Typing)
	assert.False(t, capabilities.SubjectAndCC)
}

func TestMetaInboundServiceWindowOpensAndExpires(t *testing.T) {
	t.Parallel()

	serverAcceptedAt := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	providerTime := serverAcceptedAt.Add(-3 * time.Hour)
	windowOpenedAt := InboundServiceWindowAnchor(
		serverAcceptedAt,
		providerTime,
		serverAcceptedAt.Add(time.Hour),
	)
	assert.Equal(t, providerTime, windowOpenedAt)

	endsAt := InboundServiceWindowEndsAt(models.ChannelInstagram, windowOpenedAt)
	require.NotNil(t, endsAt)
	assert.Equal(t, providerTime.Add(MetaCustomerServiceWindow), *endsAt)
	assert.Nil(t, InboundServiceWindowEndsAt(models.ChannelEmail, windowOpenedAt))

	capabilities := ApplyMandatoryProviderCapabilities(
		models.ChannelInstagram,
		Capabilities{Text: true},
	)
	parts := []MessagePart{{
		Type: models.MessagePartTypeText,
		Text: "A service reply",
	}}
	require.NoError(t, ValidateServiceWindow(
		capabilities,
		endsAt,
		parts,
		endsAt.Add(-time.Second),
	))
	assert.Error(t, ValidateServiceWindow(
		capabilities,
		endsAt,
		parts,
		*endsAt,
	))
}

func relayMessageBody(t *testing.T, message InboundMessage) []byte {
	t.Helper()

	body, err := json.Marshal(relayWebhookEnvelope{
		ExternalAccountID: "page-123",
		Events: []InboundEvent{{
			DedupeKey: "event-1",
			Type:      NormalizedEventTypeMessage,
			Message:   &message,
		}},
	})
	require.NoError(t, err)
	return body
}

func validRelayInboundMessage(part MessagePart) InboundMessage {
	return InboundMessage{
		ExternalMessageID: "message-1",
		Conversation:      ConversationRef{ExternalID: "thread-1"},
		Sender: Participant{
			ExternalID: "customer-1",
			Role:       models.ConversationParticipantRoleCustomer,
		},
		Direction: models.DirectionIncoming,
		Parts:     []MessagePart{part},
	}
}
