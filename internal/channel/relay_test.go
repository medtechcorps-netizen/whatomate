package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayTestEncryptionKey = "relay-test-encryption-key-at-least-32-bytes"
const relayTestProviderProofSecret = "relay-test-provider-proof-secret-at-least-32-bytes"

func TestMetaProviderProofKeyIDIsDomainSeparatedAndValidated(t *testing.T) {
	keyID, err := MetaProviderProofKeyID(relayTestProviderProofSecret)
	require.NoError(t, err)
	assert.Regexp(t, `^sha256=[0-9a-f]{64}$`, keyID)
	assert.NotEqual(
		t,
		SignMetaProviderInboundProof(relayTestProviderProofSecret, nil),
		keyID,
	)
	assert.NotEqual(
		t,
		SignMetaProviderOutboundProof(relayTestProviderProofSecret, nil),
		keyID,
	)

	_, err = MetaProviderProofKeyID("too-short")
	require.ErrorIs(t, err, ErrRelayMetaProviderProofSecret)
	_, err = MetaProviderProofKeyID(relayTestProviderProofSecret + " ")
	require.ErrorIs(t, err, ErrRelayMetaProviderProofSecret)
}

const relayTestMetaBusinessID = "200000000000001"

func TestRelayAdapterVerifyWebhookFailsClosed(t *testing.T) {
	t.Parallel()

	adapter := NewRelayAdapter(models.ChannelInstagram, nil, relayTestEncryptionKey).
		WithExpectedMetaBusinessID(relayTestMetaBusinessID).
		WithMetaProviderProofSecret(relayTestProviderProofSecret)
	account := relayTestAccount(t, "https://relay.example.test/events")
	body := []byte(`{"external_account_id":"page-123","events":[]}`)

	assert.ErrorIs(t, adapter.VerifyWebhook(account, http.Header{}, body), ErrRelaySignatureMissing)

	invalidHeaders := http.Header{}
	invalidHeaders.Set(RelaySignatureHeader, "sha256=invalid")
	assert.ErrorIs(t, adapter.VerifyWebhook(account, invalidHeaders, body), ErrRelaySignatureInvalid)

	validHeaders := http.Header{}
	validHeaders.Set(RelaySignatureHeader, signRelayBody("inbound-secret", body))
	validHeaders.Set(
		RelayMetaProviderProofHeader,
		SignMetaProviderInboundProof(relayTestProviderProofSecret, body),
	)
	require.NoError(t, adapter.VerifyWebhook(account, validHeaders, body))

	tenantSignatureOnly := validHeaders.Clone()
	tenantSignatureOnly.Del(RelayMetaProviderProofHeader)
	assert.ErrorIs(
		t,
		adapter.VerifyWebhook(account, tenantSignatureOnly, body),
		ErrRelayMetaProviderProofMissing,
	)

	wrongProviderProof := validHeaders.Clone()
	wrongProviderProof.Set(
		RelayMetaProviderProofHeader,
		SignMetaProviderInboundProof("different-provider-proof-secret-at-least-32-bytes", body),
	)
	assert.ErrorIs(
		t,
		adapter.VerifyWebhook(account, wrongProviderProof, body),
		ErrRelayMetaProviderProofInvalid,
	)

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

func TestRelayAdapterRequiresExactMetaProductionReadinessAttestation(t *testing.T) {
	t.Parallel()

	accountID := uuid.MustParse("00000000-0000-4000-8000-000000000123")
	organizationID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	for _, testCase := range []struct {
		name   string
		mutate func(http.Header)
		valid  bool
	}{
		{name: "exact mapping", valid: true},
		{
			name: "missing readiness version",
			mutate: func(headers http.Header) {
				headers.Del(RelayReadinessHeader)
			},
		},
		{
			name: "obsolete readiness version",
			mutate: func(headers http.Header) {
				headers.Set(RelayReadinessHeader, "v1")
			},
		},
		{
			name: "wrong external asset",
			mutate: func(headers http.Header) {
				headers.Set(RelayExternalAccountHeader, "some-other-page")
			},
		},
		{
			name: "wrong tenant channel account mapping",
			mutate: func(headers http.Header) {
				headers.Set(RelayChannelAccountHeader, uuid.NewString())
			},
		},
		{
			name: "wrong organization mapping",
			mutate: func(headers http.Header) {
				headers.Set(RelayOrganizationHeader, uuid.NewString())
			},
		},
		{
			name: "missing organization mapping",
			mutate: func(headers http.Header) {
				headers.Del(RelayOrganizationHeader)
			},
		},
		{
			name: "missing Meta Business ownership",
			mutate: func(headers http.Header) {
				headers.Del(RelayMetaBusinessHeader)
			},
		},
		{
			name: "non-numeric Meta Business ownership",
			mutate: func(headers http.Header) {
				headers.Set(RelayMetaBusinessHeader, "not-numeric")
			},
		},
		{
			name: "wrong numeric Meta Business ownership",
			mutate: func(headers http.Header) {
				headers.Set(RelayMetaBusinessHeader, "200000000000002")
			},
		},
		{
			name: "missing deployment provider proof",
			mutate: func(headers http.Header) {
				headers.Del(RelayMetaProviderProofHeader)
			},
		},
		{
			name: "wrong deployment provider proof",
			mutate: func(headers http.Header) {
				headers.Set(
					RelayMetaProviderProofHeader,
					SignMetaProviderReadinessProof(
						"wrong-provider-proof-secret-at-least-32-bytes",
						models.ChannelInstagram,
						"page-123",
						accountID.String(),
						organizationID.String(),
						relayTestMetaBusinessID,
					),
				)
			},
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				assert.Equal(t, http.MethodHead, request.Method)
				assert.Equal(t, signRelayBody("outbound-secret", nil), request.Header.Get(RelaySignatureHeader))
				headers := w.Header()
				headers.Set(RelayReadinessHeader, RelayReadinessVersion)
				headers.Set(RelayChannelHeader, string(models.ChannelInstagram))
				headers.Set(RelayExternalAccountHeader, "page-123")
				headers.Set(RelayChannelAccountHeader, accountID.String())
				headers.Set(RelayOrganizationHeader, organizationID.String())
				headers.Set(RelayMetaBusinessHeader, "200000000000001")
				headers.Set(
					RelayMetaProviderProofHeader,
					SignMetaProviderReadinessProof(
						relayTestProviderProofSecret,
						models.ChannelInstagram,
						"page-123",
						accountID.String(),
						organizationID.String(),
						relayTestMetaBusinessID,
					),
				)
				if testCase.mutate != nil {
					testCase.mutate(headers)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			adapter := NewRelayAdapter(models.ChannelInstagram, server.Client(), relayTestEncryptionKey).
				WithExpectedMetaBusinessID(relayTestMetaBusinessID).
				WithMetaProviderProofSecret(relayTestProviderProofSecret)
			adapter.allowLocalhostDev = true
			account := relayTestAccount(t, server.URL)
			account.ID = accountID
			account.OrganizationID = organizationID
			result, err := adapter.ValidateAccount(context.Background(), account)
			if testCase.valid {
				require.NoError(t, err)
				assert.True(t, result.Valid)
				return
			}
			require.Error(t, err)
			assert.False(t, result.Valid)
			assert.Contains(t, strings.ToLower(err.Error()), "readiness")
		})
	}
}

func TestRelayAdapterMetaReadinessFailsClosedWithoutTrustedBusinessExpectation(t *testing.T) {
	t.Parallel()

	accountID := uuid.MustParse("00000000-0000-4000-8000-000000000123")
	organizationID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(RelayReadinessHeader, RelayReadinessVersion)
		w.Header().Set(RelayChannelHeader, string(models.ChannelInstagram))
		w.Header().Set(RelayExternalAccountHeader, "page-123")
		w.Header().Set(RelayChannelAccountHeader, accountID.String())
		w.Header().Set(RelayOrganizationHeader, organizationID.String())
		w.Header().Set(RelayMetaBusinessHeader, "200000000000001")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelInstagram, server.Client(), relayTestEncryptionKey)
	adapter.allowLocalhostDev = true
	account := relayTestAccount(t, server.URL)
	account.ID = accountID
	account.OrganizationID = organizationID
	result, err := adapter.ValidateAccount(context.Background(), account)
	require.Error(t, err)
	assert.False(t, result.Valid)
}

func TestRelayAdapterNonMetaHealthContractRemainsBackwardCompatible(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer server.Close()

	adapter := NewRelayAdapter(models.ChannelWebChat, server.Client(), relayTestEncryptionKey)
	adapter.allowLocalhostDev = true
	account := relayTestAccount(t, server.URL)
	account.Channel = models.ChannelWebChat
	result, err := adapter.ValidateAccount(context.Background(), account)
	require.NoError(t, err)
	assert.True(t, result.Valid)
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
