package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayTestEncryptionKey = "relay-test-encryption-key-at-least-32-bytes"

func TestRelayAdapterVerifyWebhookFailsClosed(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	account := relayTestAccount(t, "https://relay.example.test/events")
	body := []byte(`{"external_account_id":"page-123","events":[]}`)

	assert.ErrorIs(t, adapter.VerifyWebhook(account, http.Header{}, body), ErrRelaySignatureMissing)

	invalidHeaders := http.Header{}
	invalidHeaders.Set(RelaySignatureHeader, "sha256=invalid")
	assert.ErrorIs(t, adapter.VerifyWebhook(account, invalidHeaders, body), ErrRelaySignatureInvalid)

	validHeaders := http.Header{}
	validHeaders.Set(RelaySignatureHeader, signRelayBody("inbound-secret", body))
	require.NoError(t, adapter.VerifyWebhook(account, validHeaders, body))

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = 'x'
	assert.ErrorIs(t, adapter.VerifyWebhook(account, validHeaders, tampered), ErrRelaySignatureInvalid)
}

func TestRelayAdapterRouteAndNormalizeWebhook(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	adapter.now = func() time.Time { return now }
	account := relayTestAccount(t, "https://relay.example.test/events")
	body := []byte(`{
		"external_account_id":"page-123",
		"events":[{
			"dedupe_key":"event-1",
			"type":"message",
			"message":{
				"external_message_id":"message-1",
				"conversation":{"external_id":"thread-1"},
				"sender":{"external_id":"contact-1","display_name":"Amira","role":"customer"},
				"direction":"incoming",
				"parts":[{"type":"text","text":"Hello"}]
			}
		}]
	}`)

	hint, err := adapter.RouteHint(nil, body)
	require.NoError(t, err)
	assert.Equal(t, "page-123", hint.ExternalAccountID)
	assert.Equal(t, RelayProvider, hint.Provider)

	events, err := adapter.NormalizeWebhook(context.Background(), account, body)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].Message)
	assert.Equal(t, models.DirectionIncoming, events[0].Message.Direction)
	assert.Equal(t, now, events[0].OccurredAt)
	assert.Equal(t, now, events[0].Message.SentAt)
	assert.Equal(t, now, events[0].Message.ReceivedAt)

	duplicateBody := []byte(`{
		"external_account_id":"page-123",
		"events":[
			{
				"dedupe_key":"same",
				"type":"read",
				"read":{
					"conversation":{"external_id":"thread-1"},
					"external_message_ids":["message-1"],
					"reader":{"external_id":"contact-1"}
				}
			},
			{
				"dedupe_key":"same",
				"type":"read",
				"read":{
					"conversation":{"external_id":"thread-1"},
					"external_message_ids":["message-1"],
					"reader":{"external_id":"contact-1"}
				}
			}
		]
	}`)
	_, err = adapter.NormalizeWebhook(context.Background(), account, duplicateBody)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicates dedupe_key")
}

func TestRelayAdapterSendSignsRequest(t *testing.T) {
	t.Parallel()

	var received relayOutboundEnvelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			assert.NoError(t, r.Body.Close())
		}()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, signRelayBody("outbound-secret", body), r.Header.Get(RelaySignatureHeader))
		require.NoError(t, json.Unmarshal(body, &received))
		assert.Equal(t, "message", received.Type)
		data, ok := received.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "outbox-1", data["idempotency_key"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"provider_message_ids":["provider-message-1"],
			"external_conversation_id":"thread-1",
			"provider_request_id":"request-1"
		}`))
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelWebChat, server.Client(), relayTestEncryptionKey)
	adapter.allowLocalhostDev = true
	account := relayTestAccount(t, server.URL)
	account.Channel = models.ChannelWebChat
	account.Config["outbound_enabled"] = true

	result, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-1",
		Conversation:   ConversationRef{ExternalID: "thread-1"},
		Recipient:      Participant{ExternalID: "contact-1"},
		Parts: []MessagePart{{
			Type: models.MessagePartTypeText,
			Text: "Hello",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"provider-message-1"}, result.ProviderMessageIDs)
	assert.Equal(t, "request-1", result.ProviderRequestID)
	assert.Equal(t, models.MessageStatusSent, result.Status)
}

func TestRelayAdapterRejectsUnsupportedEventType(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	account := relayTestAccount(t, "https://relay.example.test/events")
	body := []byte(`{
		"external_account_id":"page-123",
		"events":[{
			"dedupe_key":"event-unsupported",
			"type":"provider_private_extension"
		}]
	}`)

	_, err := adapter.NormalizeWebhook(context.Background(), account, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not supported")
}

func TestRelayAdapterRejectsExpiredCredential(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey)
	adapter.now = func() time.Time { return now }
	account := relayTestAccount(t, "https://relay.example.test/events")
	expired := now.Add(-time.Second)
	account.Credentials[0].ExpiresAt = &expired
	body := []byte(`{"external_account_id":"page-123","events":[]}`)
	headers := http.Header{}
	headers.Set(RelaySignatureHeader, signRelayBody("inbound-secret", body))

	assert.ErrorIs(t, adapter.VerifyWebhook(account, headers, body), ErrRelaySecretMissing)
}

func TestRelayAdapterSendRequiresApproval(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelEmail, nil, relayTestEncryptionKey)
	account := relayTestAccount(t, "https://relay.example.test/events")
	_, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-1",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
	})
	assert.ErrorIs(t, err, ErrRelayOutboundDisabled)
}

func TestRelayAdapterIgnoresTenantLocalhostBypass(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelWebChat, server.Client(), relayTestEncryptionKey)
	account := relayTestAccount(t, server.URL)
	account.Channel = models.ChannelWebChat
	account.Config["allow_localhost_dev"] = true
	account.Config["outbound_enabled"] = true

	_, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-tenant-bypass",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)
	assert.Zero(t, requests.Load())
}

func TestValidateRelayURL(t *testing.T) {
	t.Parallel()

	parsed, err := validateRelayURL("https://relay.example.test/events", false)
	require.NoError(t, err)
	assert.Equal(t, "relay.example.test", parsed.Hostname())

	_, err = validateRelayURL("http://relay.example.test/events", true)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)

	_, err = validateRelayURL("http://127.0.0.1:8080/events", false)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)

	parsed, err = validateRelayURL("http://127.0.0.1:8080/events", true)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", parsed.Hostname())

	_, err = validateRelayURL("https://10.0.0.3/events", false)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)

	_, err = validateRelayURL("https://relay.example.test/events?token=secret", false)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)
}

func TestRelayAdapterBlocksHostnameResolvingToPrivateAddress(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelWebChat, &http.Client{Timeout: time.Second}, relayTestEncryptionKey)
	adapter.lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		assert.Equal(t, "private-relay.example", host)
		return []net.IPAddr{{IP: net.ParseIP("10.2.3.4")}}, nil
	}
	account := relayTestAccount(t, "https://private-relay.example/events")
	account.Channel = models.ChannelWebChat
	account.Config["outbound_enabled"] = true

	_, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-private",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRelayURLInvalid)
}

func TestRelayAdapterDoesNotReplaySignedRequestAcrossRedirect(t *testing.T) {
	t.Parallel()

	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get(RelaySignatureHeader))
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelWebChat, server.Client(), relayTestEncryptionKey)
	adapter.allowLocalhostDev = true
	account := relayTestAccount(t, server.URL)
	account.Channel = models.ChannelWebChat
	account.Config["outbound_enabled"] = true

	_, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-redirect",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 307")
	assert.Zero(t, destinationRequests.Load(), "signed relay payload must not be replayed")
}

func TestRelayHTTPErrorDoesNotExposeResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider_secret=do-not-expose", http.StatusBadRequest)
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelWebChat, server.Client(), relayTestEncryptionKey)
	adapter.allowLocalhostDev = true
	account := relayTestAccount(t, server.URL)
	account.Channel = models.ChannelWebChat
	account.Config["outbound_enabled"] = true

	_, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey: "outbox-error",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "do-not-expose")
	assert.Contains(t, err.Error(), "HTTP 400")
}

func relayTestAccount(t *testing.T, relayURL string) *models.ChannelAccount {
	t.Helper()

	inbound, err := appcrypto.Encrypt("inbound-secret", relayTestEncryptionKey)
	require.NoError(t, err)
	outbound, err := appcrypto.Encrypt("outbound-secret", relayTestEncryptionKey)
	require.NoError(t, err)

	return &models.ChannelAccount{
		Channel:           models.ChannelInstagram,
		Provider:          RelayProvider,
		ExternalAccountID: "page-123",
		Config: models.JSONB{
			"relay_url": relayURL,
		},
		Credentials: []models.ChannelCredential{{
			Kind:   models.ChannelCredentialKindWebhook,
			Status: models.ChannelCredentialStatusActive,
			CredentialBlob: models.JSONB{
				"inbound_secret":  inbound,
				"outbound_secret": outbound,
			},
		}},
	}
}

func TestRelayAdapterCredentialRefreshIsExplicitlyUnsupported(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelSMS, nil, relayTestEncryptionKey)
	_, err := adapter.RefreshCredentials(context.Background(), relayTestAccount(t, "https://relay.example.test"))
	assert.True(t, errors.Is(err, ErrCredentialRefreshUnsupported))
}
