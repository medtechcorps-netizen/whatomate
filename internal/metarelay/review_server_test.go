package metarelay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

func TestReviewServerAcceptsOnlyWhitelistedMessengerPageWithBrokerBinding(t *testing.T) {
	config := newReviewTestConfig(t)
	now := time.Now().UTC()
	resolver := &fakeReviewResolver{binding: validReviewBinding(config, now)}
	store := newMemoryServerStore()
	server, err := NewServer(
		config,
		store,
		WithServerReviewBindingResolver(resolver),
	)
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	server.now = func() time.Time { return now }
	raw := reviewMessengerPayload(config.ReviewPageID)
	request := httptest.NewRequest(http.MethodPost, "/v1/meta/messenger/webhook", bytes.NewReader(raw))
	request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, raw))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "EVENT_RECEIVED" {
		t.Fatalf("review webhook status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.callCount() != 1 {
		t.Fatalf("broker resolution calls=%d, want 1", resolver.callCount())
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.acceptCalls != 1 || len(store.accepted) != 1 {
		t.Fatalf("accept calls=%d records=%d", store.acceptCalls, len(store.accepted))
	}
	var jobs []InboundJob
	for _, accepted := range store.accepted {
		jobs = accepted
	}
	if len(jobs) != 1 {
		t.Fatalf("accepted jobs=%d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.AccountKey != config.ReviewChannelAccountID ||
		job.ReviewGeneration != config.ReviewGeneration ||
		job.ReviewCredentialID != resolver.binding.CredentialID ||
		job.ReviewCredentialVersion != resolver.binding.CredentialVersion {
		t.Fatalf("job binding fence is incomplete: %+v", job)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("encode accepted job: %v", err)
	}
	for _, secret := range []string{
		resolver.binding.InboundSecret,
		config.ReviewBrokerAuthSecret,
		config.ReviewBundleEncryptionSecret,
		config.MessengerAppSecret,
	} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("durable job contains secret material")
		}
	}
}

func TestReviewServerRejectsForeignPageBeforeBrokerOrRedis(t *testing.T) {
	config := newReviewTestConfig(t)
	resolver := &fakeReviewResolver{binding: validReviewBinding(config, time.Now().UTC())}
	store := newMemoryServerStore()
	server, err := NewServer(config, store, WithServerReviewBindingResolver(resolver))
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	raw := reviewMessengerPayload("999999999999999")
	request := httptest.NewRequest(http.MethodPost, "/v1/meta/messenger/webhook", bytes.NewReader(raw))
	request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, raw))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign Page status=%d, want 404", response.Code)
	}
	if resolver.callCount() != 0 || store.acceptCalls != 0 {
		t.Fatalf("foreign Page reached broker/store: broker=%d store=%d", resolver.callCount(), store.acceptCalls)
	}
}

func TestReviewServerBrokerOutageRequestsMetaRetry(t *testing.T) {
	config := newReviewTestConfig(t)
	resolver := &fakeReviewResolver{err: ErrReviewBindingUnavailable}
	store := newMemoryServerStore()
	server, err := NewServer(config, store, WithServerReviewBindingResolver(resolver))
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	raw := reviewMessengerPayload(config.ReviewPageID)
	request := httptest.NewRequest(http.MethodPost, "/v1/meta/messenger/webhook", bytes.NewReader(raw))
	request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, raw))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || store.acceptCalls != 0 {
		t.Fatalf("broker outage status=%d accept calls=%d", response.Code, store.acceptCalls)
	}
}

func TestReviewServerDoesNotExposeInstagramHealthOrOutboundRoutes(t *testing.T) {
	config := newReviewTestConfig(t)
	resolver := &fakeReviewResolver{binding: validReviewBinding(config, time.Now().UTC())}
	store := newMemoryServerStore()
	server, err := NewServer(config, store, WithServerReviewBindingResolver(resolver))
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/meta/instagram/webhook"},
		{method: http.MethodPost, path: "/v1/meta/instagram/webhook"},
		{method: http.MethodHead, path: "/v1/meta/messenger/webhook"},
		{method: http.MethodHead, path: "/livez"},
		{method: http.MethodHead, path: "/readyz"},
		{method: http.MethodHead, path: "/v1/accounts/messenger/" + config.ReviewPageID},
		{method: http.MethodPost, path: "/v1/accounts/messenger/" + config.ReviewPageID},
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(
			response,
			httptest.NewRequest(testCase.method, testCase.path, nil),
		)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status=%d, want 404", testCase.method, testCase.path, response.Code)
		}
	}

	// Defense in depth: direct calls also reject before account lookup, secret
	// verification, idempotency storage, or any Graph side effect.
	direct := httptest.NewRecorder()
	server.handleReReplyOutbound(
		direct,
		httptest.NewRequest(http.MethodPost, "/direct", strings.NewReader(`{}`)),
	)
	if direct.Code != http.StatusNotFound || store.completeCalls != 0 ||
		store.releaseCalls != 0 || store.ambiguousCalls != 0 {
		t.Fatal("direct outbound review guard did not fail before side effects")
	}
	direct = httptest.NewRecorder()
	server.handleAccountHealth(direct, httptest.NewRequest(http.MethodHead, "/direct", nil))
	if direct.Code != http.StatusNotFound {
		t.Fatal("direct health review guard did not fail closed")
	}
	var graphCalls atomic.Int32
	server.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		graphCalls.Add(1)
		return nil, errTestTransport
	})}
	account, err := resolver.binding.account()
	if err != nil {
		t.Fatalf("review account: %v", err)
	}
	result := server.sendGraph(context.Background(), account, channelapi.OutboundMessage{})
	if result.status != http.StatusNotFound {
		t.Fatalf("direct Graph send status=%d, want 404", result.status)
	}
	if err := server.validateGraphBinding(context.Background(), account); err == nil {
		t.Fatal("direct Graph binding validation was not blocked")
	}
	if err := server.validateWebhookSubscription(context.Background(), account); err == nil {
		t.Fatal("direct Graph subscription validation was not blocked")
	}
	if graphCalls.Load() != 0 {
		t.Fatalf("review mode made %d direct Graph calls", graphCalls.Load())
	}
}

func TestReviewServerAndWorkerRequireResolver(t *testing.T) {
	config := newReviewTestConfig(t)
	if _, err := NewServer(config, newMemoryServerStore()); err == nil {
		t.Fatal("review server accepted a missing broker resolver")
	}
	if _, err := NewWorker(config, &fakeQueueStore{}); err == nil {
		t.Fatal("review worker accepted a missing broker resolver")
	}
}

func TestReviewBindingReadinessValidatesExactCurrentBinding(t *testing.T) {
	config := newReviewTestConfig(t)
	now := time.Now().UTC()
	resolver := &fakeReviewResolver{binding: validReviewBinding(config, now)}
	server, err := NewServer(
		config,
		newMemoryServerStore(),
		WithServerReviewBindingResolver(resolver),
	)
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	server.now = func() time.Time { return now }
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/reviewz", nil),
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("review readiness status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.callCount() != 1 {
		t.Fatalf("binding resolutions=%d, want one", resolver.callCount())
	}
	if response.Header().Get(channelapi.RelayMetaProviderProofKeyIDHeader) == "" {
		t.Fatal("review readiness omitted provider-proof key ID")
	}
}

func TestReviewBindingReadinessReturnsOnlyGenericFailure(t *testing.T) {
	now := time.Now().UTC()
	for _, testCase := range []struct {
		name   string
		mutate func(*Config, *ReviewBinding, *fakeReviewResolver)
	}{
		{
			name: "broker unavailable",
			mutate: func(_ *Config, _ *ReviewBinding, resolver *fakeReviewResolver) {
				resolver.err = ErrReviewBindingUnavailable
			},
		},
		{
			name: "deprovisioned",
			mutate: func(_ *Config, _ *ReviewBinding, resolver *fakeReviewResolver) {
				resolver.err = ErrReviewBindingRejected
			},
		},
		{
			name: "expired",
			mutate: func(_ *Config, binding *ReviewBinding, _ *fakeReviewResolver) {
				binding.ExpiresAt = now.Add(-time.Nanosecond)
			},
		},
		{
			name: "wrong callback",
			mutate: func(_ *Config, binding *ReviewBinding, _ *fakeReviewResolver) {
				binding.ReReplyWebhookURL = "https://attacker.example/review"
			},
		},
		{
			name: "wrong generation",
			mutate: func(_ *Config, binding *ReviewBinding, _ *fakeReviewResolver) {
				binding.Tuple.Generation = "88888888-8888-4888-8888-888888888888"
			},
		},
		{
			name: "invalid inbound key",
			mutate: func(_ *Config, binding *ReviewBinding, _ *fakeReviewResolver) {
				binding.InboundSecret = "too-short"
			},
		},
		{
			name: "invalid provider proof key",
			mutate: func(config *Config, _ *ReviewBinding, _ *fakeReviewResolver) {
				config.ReReplyProviderProofSecret = "too-short"
			},
		},
		{
			name: "Messenger App Secret mismatch",
			mutate: func(config *Config, _ *ReviewBinding, _ *fakeReviewResolver) {
				config.MessengerAppSecret = "different-review-Messenger-app-secret-at-least-32-bytes"
			},
		},
		{
			name: "provider-proof secret mismatch",
			mutate: func(config *Config, _ *ReviewBinding, _ *fakeReviewResolver) {
				config.ReReplyProviderProofSecret = "different-review-provider-proof-secret-at-least-32-bytes"
			},
		},
		{
			name: "invalid credential ID",
			mutate: func(_ *Config, binding *ReviewBinding, _ *fakeReviewResolver) {
				binding.CredentialID = "not-a-credential-id"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := newReviewTestConfig(t)
			binding := validReviewBinding(config, now)
			resolver := &fakeReviewResolver{binding: binding}
			testCase.mutate(config, &resolver.binding, resolver)
			server, err := NewServer(
				config,
				newMemoryServerStore(),
				WithServerReviewBindingResolver(resolver),
			)
			if err != nil {
				t.Fatalf("new review server: %v", err)
			}
			server.now = func() time.Time { return now }
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "/reviewz", nil),
			)
			if response.Code != http.StatusServiceUnavailable ||
				strings.TrimSpace(response.Body.String()) != `{"error":"not_ready"}` {
				t.Fatalf("status=%d body=%q, want generic 503", response.Code, response.Body.String())
			}
		})
	}
}

func TestReviewWebhookRejectsSharedSecretMismatchBeforeDurableAcceptance(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "Messenger App Secret",
			mutate: func(config *Config) {
				config.MessengerAppSecret = "different-review-Messenger-app-secret-at-least-32-bytes"
			},
		},
		{
			name: "provider-proof secret",
			mutate: func(config *Config) {
				config.ReReplyProviderProofSecret = "different-review-provider-proof-secret-at-least-32-bytes"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := newReviewTestConfig(t)
			now := time.Now().UTC()
			binding := validReviewBinding(config, now)
			testCase.mutate(config)
			resolver := &fakeReviewResolver{binding: binding}
			store := newMemoryServerStore()
			server, err := NewServer(
				config,
				store,
				WithServerReviewBindingResolver(resolver),
			)
			if err != nil {
				t.Fatalf("new review server: %v", err)
			}
			server.now = func() time.Time { return now }
			raw := reviewMessengerPayload(config.ReviewPageID)
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/meta/messenger/webhook",
				bytes.NewReader(raw),
			)
			request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, raw))
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusNotFound ||
				!strings.Contains(response.Body.String(), "review_binding_rejected") {
				t.Fatalf("status=%d body=%q, want rejected binding", response.Code, response.Body.String())
			}
			if resolver.callCount() != 1 || store.acceptCalls != 0 {
				t.Fatalf("resolver calls=%d accept calls=%d", resolver.callCount(), store.acceptCalls)
			}
		})
	}
}

func TestServiceReadinessDoesNotDeadlockReviewBootstrap(t *testing.T) {
	config := newReviewTestConfig(t)
	resolver := &fakeReviewResolver{err: ErrReviewBindingRejected}
	server, err := NewServer(
		config,
		newMemoryServerStore(),
		WithServerReviewBindingResolver(resolver),
	)
	if err != nil {
		t.Fatalf("new review server: %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/readyz", nil),
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("bootstrap service readiness status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.callCount() != 0 {
		t.Fatalf("service readiness made %d broker calls", resolver.callCount())
	}
}

func TestReviewReadinessRouteIsAbsentInProduction(t *testing.T) {
	server, err := NewServer(newTestConfig(t), newMemoryServerStore())
	if err != nil {
		t.Fatalf("new production server: %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/reviewz", nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("production review readiness status=%d, want 404", response.Code)
	}
}

func reviewMessengerPayload(pageID string) []byte {
	payload := map[string]any{
		"object": "page",
		"entry": []any{map[string]any{
			"id":   pageID,
			"time": int64(1_786_310_400),
			"messaging": []any{map[string]any{
				"sender":    map[string]string{"id": "review-customer-1"},
				"recipient": map[string]string{"id": pageID},
				"timestamp": int64(1_786_310_400_000),
				"message": map[string]any{
					"mid":  "review-message-1",
					"text": "Messenger App Review test",
				},
			}},
		}},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}
