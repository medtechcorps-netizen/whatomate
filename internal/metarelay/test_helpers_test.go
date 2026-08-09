package metarelay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

const testMetaProviderProofSecret = "test-meta-provider-proof-secret-at-least-32-bytes"

func newTestConfig(t *testing.T) *Config {
	t.Helper()
	reviewedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	config := &Config{
		ListenAddr:                  ":0",
		RedisURL:                    "redis://localhost:6379/0",
		RedisPrefix:                 "test:meta-relay:",
		ReReplyBaseURL:              "https://rereply.example.test",
		ReReplyProviderProofSecret:  testMetaProviderProofSecret,
		MessengerAppSecret:          "messenger-app-secret",
		MessengerAppID:              "100000000000001",
		MessengerAppMode:            "live",
		MessengerAppOwnerBusinessID: "300000000000001",
		MessengerTechProviderStatus: "verified",
		MessengerAppReviewStatus:    "approved",
		MessengerAppPermissions: []string{
			"pages_messaging",
			"pages_manage_metadata",
			"instagram_basic",
			"instagram_manage_messages",
			"pages_show_list",
		},
		MessengerReviewedBy:         "Test Governance Reviewer <governance@example.test>",
		MessengerReviewedAt:         reviewedAt,
		MessengerReviewEvidence:     "https://evidence.example.test/meta/messenger",
		MessengerVerifyToken:        "messenger-verify-token",
		InstagramLoginAppSecret:     "instagram-app-secret",
		InstagramLoginAppID:         "100000000000002",
		InstagramAppMode:            "live",
		InstagramAppOwnerBusinessID: "300000000000001",
		InstagramTechProviderStatus: "verified",
		InstagramAppReviewStatus:    "approved",
		InstagramAppPermissions: []string{
			"instagram_business_basic",
			"instagram_business_manage_messages",
		},
		InstagramReviewedBy:        "Test Governance Reviewer <governance@example.test>",
		InstagramReviewedAt:        reviewedAt,
		InstagramReviewEvidence:    "https://evidence.example.test/meta/instagram",
		InstagramLoginVerifyToken:  "instagram-verify-token",
		GraphAPIVersion:            "v25.0",
		InboundRetention:           time.Hour,
		OutboundRetention:          time.Hour,
		ProcessingLease:            30 * time.Second,
		ForwardTimeout:             10 * time.Second,
		PollInterval:               time.Millisecond,
		WorkerConcurrency:          4,
		MaxAttempts:                3,
		allowInsecureTestEndpoints: true,
		Accounts: []AccountConfig{
			{
				Key:                      "messenger-page",
				OrganizationID:           "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1",
				MetaBusinessID:           "200000000000001",
				Channel:                  models.ChannelMessenger,
				ExternalAccountID:        "100000000000010",
				ReReplyWebhookURL:        "https://rereply.example.test/api/webhooks/channels/00000000-0000-4000-8000-000000000001",
				AccessTokenEnv:           "TEST_MESSENGER_TOKEN",
				ReReplyInboundSecretEnv:  "TEST_MESSENGER_INBOUND",
				ReReplyOutboundSecretEnv: "TEST_MESSENGER_OUTBOUND",
			},
			{
				Key:                      "instagram-direct",
				OrganizationID:           "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2",
				MetaBusinessID:           "200000000000002",
				Channel:                  models.ChannelInstagram,
				ExternalAccountID:        "17841400000000001",
				InstagramAPIMode:         InstagramAPIModeInstagramLogin,
				ReReplyWebhookURL:        "https://rereply.example.test/api/webhooks/channels/00000000-0000-4000-8000-000000000002",
				AccessTokenEnv:           "TEST_IG_DIRECT_TOKEN",
				ReReplyInboundSecretEnv:  "TEST_IG_DIRECT_INBOUND",
				ReReplyOutboundSecretEnv: "TEST_IG_DIRECT_OUTBOUND",
			},
			{
				Key:                      "instagram-page",
				OrganizationID:           "cccccccc-cccc-4ccc-8ccc-ccccccccccc3",
				MetaBusinessID:           "200000000000003",
				Channel:                  models.ChannelInstagram,
				ExternalAccountID:        "17841400000000002",
				InstagramAPIMode:         InstagramAPIModeFacebookLogin,
				ReReplyWebhookURL:        "https://rereply.example.test/api/webhooks/channels/00000000-0000-4000-8000-000000000003",
				AccessTokenEnv:           "TEST_IG_PAGE_TOKEN",
				ReReplyInboundSecretEnv:  "TEST_IG_PAGE_INBOUND",
				ReReplyOutboundSecretEnv: "TEST_IG_PAGE_OUTBOUND",
			},
		},
	}
	secrets := map[string]string{
		"TEST_MESSENGER_TOKEN":    "messenger-access-token",
		"TEST_MESSENGER_INBOUND":  "messenger-inbound-secret-at-least-32-bytes",
		"TEST_MESSENGER_OUTBOUND": "messenger-outbound-secret-at-least-32-bytes",
		"TEST_IG_DIRECT_TOKEN":    "instagram-direct-access-token",
		"TEST_IG_DIRECT_INBOUND":  "instagram-direct-inbound-secret-at-least-32-bytes",
		"TEST_IG_DIRECT_OUTBOUND": "instagram-direct-outbound-secret-at-least-32-bytes",
		"TEST_IG_PAGE_TOKEN":      "instagram-page-access-token",
		"TEST_IG_PAGE_INBOUND":    "instagram-page-inbound-secret-at-least-32-bytes",
		"TEST_IG_PAGE_OUTBOUND":   "instagram-page-outbound-secret-at-least-32-bytes",
	}
	if err := config.validateAndIndex(func(name string) string { return secrets[name] }); err != nil {
		t.Fatalf("validate test config: %v", err)
	}
	return config
}

type fakeOutboundRecord struct {
	digest string
	state  OutboundClaimState
	status int
	result []byte
}

type memoryServerStore struct {
	mu sync.Mutex

	pingErr        error
	acceptErr      error
	accepted       map[string][]InboundJob
	acceptCalls    int
	outbound       map[string]fakeOutboundRecord
	completeErr    error
	releaseErr     error
	ambiguousErr   error
	completeCalls  int
	releaseCalls   int
	ambiguousCalls int
}

func newMemoryServerStore() *memoryServerStore {
	return &memoryServerStore{
		accepted: make(map[string][]InboundJob),
		outbound: make(map[string]fakeOutboundRecord),
	}
}

func (s *memoryServerStore) Ping(context.Context) error {
	return s.pingErr
}

func (s *memoryServerStore) AcceptInbound(
	_ context.Context,
	acceptanceID string,
	jobs []InboundJob,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acceptCalls++
	if s.acceptErr != nil {
		return false, s.acceptErr
	}
	if _, exists := s.accepted[acceptanceID]; exists {
		return false, nil
	}
	s.accepted[acceptanceID] = append([]InboundJob(nil), jobs...)
	return true, nil
}

func (s *memoryServerStore) ClaimOutbound(
	_ context.Context,
	key, digest string,
	_ time.Time,
) (OutboundClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.outbound[key]
	if !exists {
		s.outbound[key] = fakeOutboundRecord{
			digest: digest,
			state:  OutboundClaimInFlight,
		}
		return OutboundClaim{State: OutboundClaimAcquired}, nil
	}
	if record.digest != digest {
		return OutboundClaim{State: OutboundClaimCollision}, nil
	}
	return OutboundClaim{
		State:  record.state,
		Status: record.status,
		Result: append(json.RawMessage(nil), record.result...),
	}, nil
}

func (s *memoryServerStore) CompleteOutbound(
	_ context.Context,
	key, digest string,
	status int,
	result []byte,
	rejected bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	if s.completeErr != nil {
		return s.completeErr
	}
	record, exists := s.outbound[key]
	if !exists || record.digest != digest || record.state != OutboundClaimInFlight {
		return ErrOutboundStoreFailure
	}
	record.state = OutboundClaimCompleted
	if rejected {
		record.state = OutboundClaimRejected
	}
	record.status = status
	record.result = append([]byte(nil), result...)
	s.outbound[key] = record
	return nil
}

func (s *memoryServerStore) ReleaseOutbound(
	_ context.Context,
	key, digest string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	if s.releaseErr != nil {
		return s.releaseErr
	}
	record, exists := s.outbound[key]
	if !exists || record.digest != digest || record.state != OutboundClaimInFlight {
		return ErrOutboundStoreFailure
	}
	delete(s.outbound, key)
	return nil
}

func (s *memoryServerStore) MarkOutboundAmbiguous(
	_ context.Context,
	key, digest string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ambiguousCalls++
	if s.ambiguousErr != nil {
		return s.ambiguousErr
	}
	record, exists := s.outbound[key]
	if !exists || record.digest != digest || record.state != OutboundClaimInFlight {
		return ErrOutboundStoreFailure
	}
	record.state = OutboundClaimAmbiguous
	s.outbound[key] = record
	return nil
}

func outboundEnvelopeBody(
	t *testing.T,
	account *AccountConfig,
	idempotencyKey, text string,
) []byte {
	t.Helper()
	body, err := json.Marshal(relayOutboundEnvelope{
		Type:              "message",
		Channel:           account.Channel,
		ExternalAccountID: account.ExternalAccountID,
		Data: mustJSON(t, channelapi.OutboundMessage{
			IdempotencyKey: idempotencyKey,
			Recipient: channelapi.Participant{
				ExternalID: "customer-1",
				Role:       models.ConversationParticipantRoleCustomer,
			},
			Parts: []channelapi.MessagePart{{
				Type: models.MessagePartTypeText,
				Text: text,
			}},
		}),
	})
	if err != nil {
		t.Fatalf("encode outbound envelope: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return raw
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

var errTestTransport = errors.New("test transport failed")
