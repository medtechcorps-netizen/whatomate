package metarelay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

func performOutbound(
	t *testing.T,
	handler http.Handler,
	account *AccountConfig,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/accounts/"+string(account.Channel)+"/"+account.ExternalAccountID,
		bytes.NewReader(body),
	)
	request.Header.Set(
		ReReplySignatureHeader,
		signBody(account.reReplyOutboundSecret, body),
	)
	request.Header.Set(
		channelapi.RelayMetaProviderProofHeader,
		channelapi.SignMetaProviderOutboundProof(testMetaProviderProofSecret, body),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestOutboundRejectsTenantHMACWithoutDeploymentProviderProof(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	store := newMemoryServerStore()
	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"must-not-send"}`))
	}))
	defer graph.Close()
	server, err := NewServer(
		config,
		store,
		withGraphBases(graph.URL, "http://instagram.invalid"),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := outboundEnvelopeBody(t, account, "tenant-hmac-only", "must not send")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/accounts/messenger/"+account.ExternalAccountID,
		bytes.NewReader(body),
	)
	request.Header.Set(ReReplySignatureHeader, signBody(account.reReplyOutboundSecret, body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "invalid_provider_proof") {
		t.Fatalf("tenant-only HMAC returned %d: %s", response.Code, response.Body.String())
	}
	if graphCalls.Load() != 0 || len(store.outbound) != 0 {
		t.Fatalf("tenant-only HMAC crossed the provider/idempotency gate: Graph=%d claims=%d", graphCalls.Load(), len(store.outbound))
	}
}

func TestOutboundEnforcesGovernanceFreshnessAtRuntimeBoundary(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	fixedNow := time.Now().UTC().Truncate(time.Second)
	config.MessengerReviewedAt = fixedNow.Add(-maxGovernanceReviewAge).Format(time.RFC3339)

	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"governance-mid"}`))
	}))
	defer graph.Close()
	server, err := NewServer(
		config,
		newMemoryServerStore(),
		withGraphBases(graph.URL, "http://instagram.invalid"),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.now = func() time.Time { return fixedNow }
	boundary := performOutbound(
		t,
		server.Handler(),
		account,
		outboundEnvelopeBody(t, account, "governance-boundary", "hello"),
	)
	if boundary.Code != http.StatusOK || graphCalls.Load() != 1 {
		t.Fatalf("boundary send returned %d, Graph calls=%d", boundary.Code, graphCalls.Load())
	}

	server.now = func() time.Time { return fixedNow.Add(time.Nanosecond) }
	expired := performOutbound(
		t,
		server.Handler(),
		account,
		outboundEnvelopeBody(t, account, "governance-expired", "hello"),
	)
	if expired.Code != http.StatusServiceUnavailable ||
		!strings.Contains(expired.Body.String(), "governance_review_stale") {
		t.Fatalf("expired send returned %d: %s", expired.Code, expired.Body.String())
	}
	if graphCalls.Load() != 1 {
		t.Fatalf("Graph was called after governance expiry; calls=%d", graphCalls.Load())
	}
}

func TestOutboundGraphHostIsBoundToAccountMode(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	var facebookCalls atomic.Int32
	var instagramCalls atomic.Int32

	facebook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		facebookCalls.Add(1)
		switch request.URL.Path {
		case "/v25.0/100000000000010/messages":
			if request.Header.Get("Authorization") != "Bearer messenger-access-token" {
				t.Errorf("wrong Messenger authorization header")
			}
		case "/v25.0/17841400000000002/messages":
			if request.Header.Get("Authorization") != "Bearer instagram-page-access-token" {
				t.Errorf("wrong Facebook Login Instagram authorization header")
			}
		default:
			t.Errorf("unexpected Facebook Graph path %q", request.URL.Path)
		}
		w.Header().Set("X-FB-Request-ID", "safe-request-id")
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"facebook-mid"}`))
	}))
	defer facebook.Close()

	instagram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		instagramCalls.Add(1)
		if request.URL.Path != "/v25.0/17841400000000001/messages" {
			t.Errorf("unexpected Instagram Graph path %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer instagram-direct-access-token" {
			t.Errorf("wrong Instagram Login authorization header")
		}
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"instagram-mid"}`))
	}))
	defer instagram.Close()

	server, err := NewServer(
		config,
		store,
		withGraphBases(facebook.URL, instagram.URL),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for index := range config.Accounts {
		account := &config.Accounts[index]
		body := outboundEnvelopeBody(t, account, "host-"+account.Key, "hello")
		response := performOutbound(t, server.Handler(), account, body)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"%s outbound returned %d (%s)",
				account.Key,
				response.Code,
				response.Body.String(),
			)
		}
		if !strings.Contains(response.Body.String(), "provider_message_ids") {
			t.Fatalf("%s returned unsafe/unexpected body: %s", account.Key, response.Body.String())
		}
	}
	if facebookCalls.Load() != 2 || instagramCalls.Load() != 1 {
		t.Fatalf(
			"Graph calls facebook=%d instagram=%d, want 2/1",
			facebookCalls.Load(),
			instagramCalls.Load(),
		)
	}
}

func TestOutboundIdempotencyCachesSuccessAndRejectsCollision(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	store := newMemoryServerStore()
	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"mid-1"}`))
	}))
	defer graph.Close()
	server, err := NewServer(
		config,
		store,
		withGraphBases(graph.URL, "http://instagram.invalid"),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	body := outboundEnvelopeBody(t, account, "stable-key", "first body")
	first := performOutbound(t, server.Handler(), account, body)
	second := performOutbound(t, server.Handler(), account, body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("idempotent statuses = %d/%d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached response changed: %s != %s", first.Body.String(), second.Body.String())
	}
	if graphCalls.Load() != 1 {
		t.Fatalf("Graph called %d times for identical request", graphCalls.Load())
	}

	collision := outboundEnvelopeBody(t, account, "stable-key", "different body")
	response := performOutbound(t, server.Handler(), account, collision)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "idempotency_key_collision") {
		t.Fatalf("collision returned %d (%s)", response.Code, response.Body.String())
	}
	if graphCalls.Load() != 1 {
		t.Fatalf("Graph called after collision; calls=%d", graphCalls.Load())
	}
}

func TestOutboundTransportAnd5xxArePersistentlyAmbiguous(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		config := newTestConfig(t)
		account, _ := config.accountByKey("messenger-page")
		store := newMemoryServerStore()
		var graphCalls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			graphCalls.Add(1)
			return nil, errTestTransport
		})}
		server, err := NewServer(config, store, WithServerHTTPClient(client))
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		body := outboundEnvelopeBody(t, account, "transport-key", "hello")
		for attempt := 0; attempt < 2; attempt++ {
			response := performOutbound(t, server.Handler(), account, body)
			if response.Code != http.StatusConflict ||
				!strings.Contains(response.Body.String(), "delivery_outcome_ambiguous") {
				t.Fatalf("attempt %d returned %d (%s)", attempt, response.Code, response.Body.String())
			}
		}
		if graphCalls.Load() != 1 || store.ambiguousCalls != 1 {
			t.Fatalf(
				"Graph calls=%d ambiguous writes=%d, want 1/1",
				graphCalls.Load(),
				store.ambiguousCalls,
			)
		}
	})

	t.Run("provider 5xx", func(t *testing.T) {
		config := newTestConfig(t)
		account, _ := config.accountByKey("messenger-page")
		store := newMemoryServerStore()
		var graphCalls atomic.Int32
		const providerBody = `{"error":{"message":"access token messenger-access-token failed"}}`
		graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			graphCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(providerBody))
		}))
		defer graph.Close()
		server, err := NewServer(
			config,
			store,
			withGraphBases(graph.URL, "http://instagram.invalid"),
		)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		body := outboundEnvelopeBody(t, account, "five-hundred-key", "hello")
		for attempt := 0; attempt < 2; attempt++ {
			response := performOutbound(t, server.Handler(), account, body)
			if response.Code != http.StatusConflict ||
				strings.Contains(response.Body.String(), "messenger-access-token") {
				t.Fatalf("attempt %d returned %d (%s)", attempt, response.Code, response.Body.String())
			}
		}
		if graphCalls.Load() != 1 || store.ambiguousCalls != 1 || store.releaseCalls != 0 {
			t.Fatalf(
				"Graph=%d ambiguous=%d releases=%d, want 1/1/0",
				graphCalls.Load(),
				store.ambiguousCalls,
				store.releaseCalls,
			)
		}
	})
}

func TestOutboundExplicit429CanRetryButCompletionFailureCannot(t *testing.T) {
	t.Run("429 releases claim", func(t *testing.T) {
		config := newTestConfig(t)
		account, _ := config.accountByKey("messenger-page")
		store := newMemoryServerStore()
		var graphCalls atomic.Int32
		graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			call := graphCalls.Add(1)
			if call == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"provider":"not accepted"}`))
				return
			}
			_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"mid-after-retry"}`))
		}))
		defer graph.Close()
		server, err := NewServer(
			config,
			store,
			withGraphBases(graph.URL, "http://instagram.invalid"),
		)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		body := outboundEnvelopeBody(t, account, "rate-limit-key", "hello")
		first := performOutbound(t, server.Handler(), account, body)
		if first.Code != http.StatusTooManyRequests || first.Header().Get("Retry-After") != "7" {
			t.Fatalf("first returned %d headers=%v body=%s", first.Code, first.Header(), first.Body.String())
		}
		second := performOutbound(t, server.Handler(), account, body)
		if second.Code != http.StatusOK {
			t.Fatalf("retry returned %d (%s)", second.Code, second.Body.String())
		}
		if graphCalls.Load() != 2 || store.releaseCalls != 1 {
			t.Fatalf("Graph=%d releases=%d, want 2/1", graphCalls.Load(), store.releaseCalls)
		}
	})

	t.Run("success without durable completion is ambiguous", func(t *testing.T) {
		config := newTestConfig(t)
		account, _ := config.accountByKey("messenger-page")
		store := newMemoryServerStore()
		store.completeErr = ErrOutboundStoreFailure
		var graphCalls atomic.Int32
		graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			graphCalls.Add(1)
			_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"mid-1"}`))
		}))
		defer graph.Close()
		server, err := NewServer(
			config,
			store,
			withGraphBases(graph.URL, "http://instagram.invalid"),
		)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		body := outboundEnvelopeBody(t, account, "completion-key", "hello")
		first := performOutbound(t, server.Handler(), account, body)
		second := performOutbound(t, server.Handler(), account, body)
		if first.Code != http.StatusConflict || second.Code != http.StatusConflict {
			t.Fatalf("statuses=%d/%d, want conflict/conflict", first.Code, second.Code)
		}
		if graphCalls.Load() != 1 || store.ambiguousCalls != 1 {
			t.Fatalf("Graph=%d ambiguous=%d, want 1/1", graphCalls.Load(), store.ambiguousCalls)
		}
	})
}

func TestGraphFailureBodyIsNeverReturned(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	const providerBody = "provider-internal-body-with-token"
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, providerBody)
	}))
	defer graph.Close()
	server, err := NewServer(
		config,
		newMemoryServerStore(),
		withGraphBases(graph.URL, "http://instagram.invalid"),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	response := performOutbound(
		t,
		server.Handler(),
		account,
		outboundEnvelopeBody(t, account, "rejected-key", "hello"),
	)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), providerBody) {
		t.Fatalf("unsafe provider response: %d %s", response.Code, response.Body.String())
	}
}

func TestOutboundClaimSurvivesCallerCancellation(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	memory := newMemoryServerStore()
	store := &rejectCancelledSettlementStore{memoryServerStore: memory}
	var graphCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(
		func(providerRequest *http.Request) (*http.Response, error) {
			graphCalls.Add(1)
			if err := providerRequest.Context().Err(); err != nil {
				t.Fatalf("provider context inherited caller cancellation: %v", err)
			}
			return graphTestResponse(
				http.StatusOK,
				`{"recipient_id":"customer-1","message_id":"mid-cancelled-before-claim"}`,
			), nil
		},
	)}
	server, err := NewServer(
		config,
		store,
		WithServerHTTPClient(client),
		withOutboundTimeouts(time.Second, time.Second),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	body := outboundEnvelopeBody(t, account, "cancelled-before-claim", "hello")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/accounts/"+string(account.Channel)+"/"+account.ExternalAccountID,
		bytes.NewReader(body),
	).WithContext(callerCtx)
	request.Header.Set(
		ReReplySignatureHeader,
		signBody(account.reReplyOutboundSecret, body),
	)
	request.Header.Set(
		channelapi.RelayMetaProviderProofHeader,
		channelapi.SignMetaProviderOutboundProof(testMetaProviderProofSecret, body),
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("got status %d (%s), want successful detached claim", response.Code, response.Body.String())
	}
	if graphCalls.Load() != 1 || memory.completeCalls != 1 {
		t.Fatalf(
			"Graph calls=%d completed settlements=%d, want 1/1",
			graphCalls.Load(),
			memory.completeCalls,
		)
	}
}

func TestOutboundDetachedClaimHasExplicitTimeout(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	memory := newMemoryServerStore()
	store := &blockingOutboundClaimStore{memoryServerStore: memory}
	var graphCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			graphCalls.Add(1)
			return graphTestResponse(http.StatusOK, `{}`), nil
		},
	)}
	server, err := NewServer(
		config,
		store,
		WithServerHTTPClient(client),
		withOutboundTimeouts(time.Second, 25*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	started := time.Now()
	response := performOutbound(
		t,
		server.Handler(),
		account,
		outboundEnvelopeBody(t, account, "bounded-claim", "hello"),
	)
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "idempotency_store_unavailable") {
		t.Fatalf("claim timeout returned %d (%s)", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("detached claim exceeded its explicit timeout: %s", elapsed)
	}
	if graphCalls.Load() != 0 {
		t.Fatalf("Graph called %d times after claim timeout", graphCalls.Load())
	}
}

func TestOutboundDeliveryAndSettlementSurviveCallerCancellation(t *testing.T) {
	testCases := []struct {
		name          string
		graphResponse func() (*http.Response, error)
		completeErr   error
		wantStatus    int
		wantState     OutboundClaimState
		wantComplete  int
		wantRelease   int
		wantAmbiguous int
	}{
		{
			name: "successful completion",
			graphResponse: func() (*http.Response, error) {
				return graphTestResponse(
					http.StatusOK,
					`{"recipient_id":"customer-1","message_id":"mid-cancelled-caller"}`,
				), nil
			},
			wantStatus:   http.StatusOK,
			wantState:    OutboundClaimCompleted,
			wantComplete: 1,
		},
		{
			name: "explicit 429 release",
			graphResponse: func() (*http.Response, error) {
				response := graphTestResponse(http.StatusTooManyRequests, `{}`)
				response.Header.Set("Retry-After", "3")
				return response, nil
			},
			wantStatus:  http.StatusTooManyRequests,
			wantRelease: 1,
		},
		{
			name: "transport ambiguity",
			graphResponse: func() (*http.Response, error) {
				return nil, errTestTransport
			},
			wantStatus:    http.StatusConflict,
			wantState:     OutboundClaimAmbiguous,
			wantAmbiguous: 1,
		},
		{
			name: "completion failure fallback ambiguity",
			graphResponse: func() (*http.Response, error) {
				return graphTestResponse(
					http.StatusOK,
					`{"recipient_id":"customer-1","message_id":"mid-completion-failure"}`,
				), nil
			},
			completeErr:   ErrOutboundStoreFailure,
			wantStatus:    http.StatusConflict,
			wantState:     OutboundClaimAmbiguous,
			wantComplete:  1,
			wantAmbiguous: 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			config := newTestConfig(t)
			account, _ := config.accountByKey("messenger-page")
			memory := newMemoryServerStore()
			memory.completeErr = testCase.completeErr
			store := &rejectCancelledSettlementStore{memoryServerStore: memory}
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			client := &http.Client{Transport: roundTripFunc(
				func(providerRequest *http.Request) (*http.Response, error) {
					cancelCaller()
					if err := providerRequest.Context().Err(); err != nil {
						t.Fatalf("provider context inherited caller cancellation: %v", err)
					}
					return testCase.graphResponse()
				},
			)}
			server, err := NewServer(
				config,
				store,
				WithServerHTTPClient(client),
				withOutboundTimeouts(time.Second, time.Second),
			)
			if err != nil {
				t.Fatalf("new server: %v", err)
			}
			body := outboundEnvelopeBody(t, account, "cancelled-caller", "hello")
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/accounts/"+string(account.Channel)+"/"+account.ExternalAccountID,
				bytes.NewReader(body),
			).WithContext(callerCtx)
			request.Header.Set(
				ReReplySignatureHeader,
				signBody(account.reReplyOutboundSecret, body),
			)
			request.Header.Set(
				channelapi.RelayMetaProviderProofHeader,
				channelapi.SignMetaProviderOutboundProof(testMetaProviderProofSecret, body),
			)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			cancelCaller()

			if response.Code != testCase.wantStatus {
				t.Fatalf(
					"got status %d (%s), want %d",
					response.Code,
					response.Body.String(),
					testCase.wantStatus,
				)
			}
			if memory.completeCalls != testCase.wantComplete ||
				memory.releaseCalls != testCase.wantRelease ||
				memory.ambiguousCalls != testCase.wantAmbiguous {
				t.Fatalf(
					"settlement calls complete/release/ambiguous=%d/%d/%d, want %d/%d/%d",
					memory.completeCalls,
					memory.releaseCalls,
					memory.ambiguousCalls,
					testCase.wantComplete,
					testCase.wantRelease,
					testCase.wantAmbiguous,
				)
			}
			record, exists := memory.outbound["messenger-page\ncancelled-caller"]
			if testCase.wantRelease > 0 {
				if exists {
					t.Fatalf("released claim still exists: %+v", record)
				}
			} else if !exists || record.state != testCase.wantState {
				t.Fatalf("settled record = %+v exists=%v, want state %q", record, exists, testCase.wantState)
			}
		})
	}
}

func TestOutboundDetachedDeliveryStillHasExplicitTimeout(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	memory := newMemoryServerStore()
	store := &rejectCancelledSettlementStore{memoryServerStore: memory}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	server, err := NewServer(
		config,
		store,
		WithServerHTTPClient(client),
		withOutboundTimeouts(25*time.Millisecond, time.Second),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	started := time.Now()
	response := performOutbound(
		t,
		server.Handler(),
		account,
		outboundEnvelopeBody(t, account, "bounded-timeout", "hello"),
	)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "delivery_outcome_ambiguous") {
		t.Fatalf("timeout returned %d (%s)", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("detached provider call exceeded its explicit timeout: %s", elapsed)
	}
	if memory.ambiguousCalls != 1 {
		t.Fatalf("ambiguous settlement calls=%d, want 1", memory.ambiguousCalls)
	}
}

type rejectCancelledSettlementStore struct {
	*memoryServerStore
}

func (s *rejectCancelledSettlementStore) ClaimOutbound(
	ctx context.Context,
	key, digest string,
	now time.Time,
) (OutboundClaim, error) {
	if err := ctx.Err(); err != nil {
		return OutboundClaim{}, err
	}
	return s.memoryServerStore.ClaimOutbound(ctx, key, digest, now)
}

func (s *rejectCancelledSettlementStore) CompleteOutbound(
	ctx context.Context,
	key, digest string,
	status int,
	result []byte,
	rejected bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryServerStore.CompleteOutbound(ctx, key, digest, status, result, rejected)
}

func (s *rejectCancelledSettlementStore) ReleaseOutbound(
	ctx context.Context,
	key, digest string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryServerStore.ReleaseOutbound(ctx, key, digest)
}

func (s *rejectCancelledSettlementStore) MarkOutboundAmbiguous(
	ctx context.Context,
	key, digest string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryServerStore.MarkOutboundAmbiguous(ctx, key, digest)
}

type blockingOutboundClaimStore struct {
	*memoryServerStore
}

func (s *blockingOutboundClaimStore) ClaimOutbound(
	ctx context.Context,
	_, _ string,
	_ time.Time,
) (OutboundClaim, error) {
	<-ctx.Done()
	return OutboundClaim{}, ctx.Err()
}

func graphTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
