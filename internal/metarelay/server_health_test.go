package metarelay

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

func TestAccountHealthEnforcesGovernanceFreshnessAtRuntimeBoundary(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	fixedNow := time.Now().UTC().Truncate(time.Second)
	config.MessengerReviewedAt = fixedNow.Add(-maxGovernanceReviewAge).Format(time.RFC3339)

	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		graphCalls.Add(1)
		switch request.URL.Path {
		case "/v25.0/me":
			_, _ = w.Write([]byte(`{"id":"100000000000010"}`))
		case "/v25.0/100000000000010/subscribed_apps":
			_, _ = w.Write([]byte(`{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`))
		default:
			http.NotFound(w, request)
		}
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
	requestHealth := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodHead,
			"/v1/accounts/messenger/"+account.ExternalAccountID,
			nil,
		)
		request.Header.Set(ReReplySignatureHeader, signBody(account.reReplyOutboundSecret, nil))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	if response := requestHealth(); response.Code != http.StatusNoContent {
		t.Fatalf("exact 90-day boundary returned %d: %s", response.Code, response.Body.String())
	}
	if graphCalls.Load() != 2 {
		t.Fatalf("Graph calls at boundary = %d, want 2", graphCalls.Load())
	}

	server.now = func() time.Time { return fixedNow.Add(time.Nanosecond) }
	response := requestHealth()
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "governance_review_stale") {
		t.Fatalf("expired runtime governance returned %d: %s", response.Code, response.Body.String())
	}
	if graphCalls.Load() != 2 {
		t.Fatalf("Graph was called after governance expiry; calls=%d", graphCalls.Load())
	}
}

func TestAccountHealthValidatesRedisTokenAccountAndGraphHost(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	var facebookCalls atomic.Int32
	var instagramCalls atomic.Int32

	facebook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		facebookCalls.Add(1)
		switch {
		case request.URL.Path == "/v25.0/me" && request.Header.Get("Authorization") == "Bearer messenger-access-token":
			if request.URL.Query().Get("fields") != "id" {
				t.Errorf("Messenger fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"id":"100000000000010"}`))
		case request.URL.Path == "/v25.0/100000000000010/subscribed_apps" && request.Header.Get("Authorization") == "Bearer messenger-access-token":
			if request.URL.Query().Get("fields") != "id,subscribed_fields" {
				t.Errorf("Messenger subscription fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`))
		case request.URL.Path == "/v25.0/me" && request.Header.Get("Authorization") == "Bearer instagram-page-access-token":
			if request.URL.Query().Get("fields") != "id,instagram_business_account{id}" {
				t.Errorf("Facebook Login Instagram fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(
				`{"id":"facebook-page-1","instagram_business_account":{"id":"17841400000000002"}}`,
			))
		case request.URL.Path == "/v25.0/17841400000000002/subscribed_apps" && request.Header.Get("Authorization") == "Bearer instagram-page-access-token":
			if request.URL.Query().Get("fields") != "id,subscribed_fields" {
				t.Errorf("Facebook Login Instagram subscription fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`))
		default:
			http.Error(w, "expired token", http.StatusUnauthorized)
		}
	}))
	defer facebook.Close()

	instagram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		instagramCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer instagram-direct-access-token" {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/v25.0/me":
			if request.URL.Query().Get("fields") != "user_id" {
				t.Errorf("Instagram Login fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"user_id":"17841400000000001"}`))
		case "/v25.0/17841400000000001/subscribed_apps":
			if request.URL.Query().Get("fields") != "id,subscribed_fields" {
				t.Errorf("Instagram Login subscription fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"100000000000002","subscribed_fields":["messages"]}]}`))
		default:
			http.Error(w, "unknown path", http.StatusNotFound)
		}
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
	expectedKeyID, err := channelapi.MetaProviderProofKeyID(
		config.ReReplyProviderProofSecret,
	)
	if err != nil {
		t.Fatalf("provider proof key ID: %v", err)
	}
	readyResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		readyResponse,
		httptest.NewRequest(http.MethodGet, "/readyz", nil),
	)
	if readyResponse.Code != http.StatusNoContent ||
		readyResponse.Header().Get(channelapi.RelayMetaProviderProofKeyIDHeader) != expectedKeyID {
		t.Fatalf("relay readiness key ID is missing: %#v", readyResponse.Header())
	}
	for _, account := range config.Accounts {
		request := httptest.NewRequest(
			http.MethodHead,
			"/v1/accounts/"+string(account.Channel)+"/"+account.ExternalAccountID,
			nil,
		)
		request.Header.Set(
			ReReplySignatureHeader,
			signBody(account.reReplyOutboundSecret, nil),
		)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf(
				"%s health returned %d (%s)",
				account.Key,
				response.Code,
				response.Body.String(),
			)
		}
		if response.Header().Get("Cache-Control") != "no-store, private" {
			t.Fatalf("%s health response is cacheable: %#v", account.Key, response.Header())
		}
		if response.Header().Get(channelapi.RelayReadinessHeader) != channelapi.RelayReadinessVersion ||
			response.Header().Get(channelapi.RelayChannelHeader) != string(account.Channel) ||
			response.Header().Get(channelapi.RelayExternalAccountHeader) != account.ExternalAccountID ||
			response.Header().Get(channelapi.RelayChannelAccountHeader) != account.reReplyChannelAccountID ||
			response.Header().Get(channelapi.RelayOrganizationHeader) != account.OrganizationID ||
			response.Header().Get(channelapi.RelayMetaBusinessHeader) != account.MetaBusinessID ||
			response.Header().Get(channelapi.RelayMetaProviderProofKeyIDHeader) != expectedKeyID ||
			response.Header().Get(channelapi.RelayMetaProviderProofHeader) !=
				channelapi.SignMetaProviderReadinessProof(
					config.ReReplyProviderProofSecret,
					account.Channel,
					account.ExternalAccountID,
					account.reReplyChannelAccountID,
					account.OrganizationID,
					account.MetaBusinessID,
				) {
			t.Fatalf("%s health readiness headers are incomplete: %#v", account.Key, response.Header())
		}
	}
	if facebookCalls.Load() != 4 {
		t.Fatalf("Facebook Graph calls = %d, want 4", facebookCalls.Load())
	}
	if instagramCalls.Load() != 2 {
		t.Fatalf("Instagram Graph calls = %d, want 2", instagramCalls.Load())
	}
}

func TestAccountHealthFailsClosedWithoutLeakingProviderData(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("instagram-direct")
	const providerSecretBody = `{"error":{"message":"token instagram-direct-access-token expired"}}`

	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "expired token",
			status: http.StatusUnauthorized,
			body:   providerSecretBody,
		},
		{
			name:   "wrong account",
			status: http.StatusOK,
			body:   `{"user_id":"some-other-instagram-account"}`,
		},
		{
			name:   "malformed response",
			status: http.StatusOK,
			body:   providerSecretBody,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(testCase.status)
					_, _ = w.Write([]byte(testCase.body))
				},
			))
			defer provider.Close()

			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			server, err := NewServer(
				config,
				newMemoryServerStore(),
				WithServerLogger(logger),
				withGraphBases("http://facebook.invalid", provider.URL),
			)
			if err != nil {
				t.Fatalf("new server: %v", err)
			}
			request := httptest.NewRequest(
				http.MethodHead,
				"/v1/accounts/instagram/"+account.ExternalAccountID,
				nil,
			)
			request.Header.Set(
				ReReplySignatureHeader,
				signBody(account.reReplyOutboundSecret, nil),
			)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("got %d (%s), want 503", response.Code, response.Body.String())
			}
			combined := logs.String() + response.Body.String()
			for _, forbidden := range []string{
				account.accessToken,
				"some-other-instagram-account",
				"token instagram-direct-access-token expired",
			} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("health output exposed %q: %s", forbidden, combined)
				}
			}
			if !strings.Contains(response.Body.String(), "account_unhealthy") {
				t.Fatalf("unexpected health body: %s", response.Body.String())
			}
		})
	}
}

func TestAccountHealthRequiresExactAppMessagesSubscription(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")

	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "wrong app",
			status: http.StatusOK,
			body:   `{"data":[{"id":"some-other-app","subscribed_fields":["messages"]}]}`,
		},
		{
			name:   "messages field missing",
			status: http.StatusOK,
			body:   `{"data":[{"id":"100000000000001","subscribed_fields":["messaging_feedback"]}]}`,
		},
		{
			name:   "provider rejected subscription query",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"private provider detail"}}`,
		},
		{
			name:   "malformed subscription response",
			status: http.StatusOK,
			body:   `{"data":`,
		},
		{
			name:   "trailing subscription response",
			status: http.StatusOK,
			body:   `{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]} {}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v25.0/me" {
					_, _ = w.Write([]byte(`{"id":"100000000000010"}`))
					return
				}
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer provider.Close()

			server, err := NewServer(
				config,
				newMemoryServerStore(),
				withGraphBases(provider.URL, "http://instagram.invalid"),
			)
			if err != nil {
				t.Fatalf("new server: %v", err)
			}
			request := httptest.NewRequest(
				http.MethodHead,
				"/v1/accounts/messenger/"+account.ExternalAccountID,
				nil,
			)
			request.Header.Set(
				ReReplySignatureHeader,
				signBody(account.reReplyOutboundSecret, nil),
			)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable ||
				!strings.Contains(response.Body.String(), "account_unhealthy") {
				t.Fatalf("got %d (%s), want generic 503", response.Code, response.Body.String())
			}
			if response.Header().Get(channelapi.RelayReadinessHeader) != "" ||
				strings.Contains(response.Body.String(), "private provider detail") {
				t.Fatalf("failed health leaked readiness or provider detail: %#v %s", response.Header(), response.Body.String())
			}
		})
	}
}

func TestAccountHealthRequiresRedisAndDoesNotProbeGraphWhenUnavailable(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	store := newMemoryServerStore()
	store.pingErr = errTestTransport
	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"100000000000010"}`))
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
	request := httptest.NewRequest(
		http.MethodHead,
		"/v1/accounts/messenger/"+account.ExternalAccountID,
		nil,
	)
	request.Header.Set(
		ReReplySignatureHeader,
		signBody(account.reReplyOutboundSecret, nil),
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", response.Code)
	}
	if graphCalls.Load() != 0 {
		t.Fatalf("Graph was probed %d times after Redis failed", graphCalls.Load())
	}
}
