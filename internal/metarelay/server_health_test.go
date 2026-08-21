package metarelay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDynamicMessengerHealthRequiresExactPlatformAppMessagesSubscription(t *testing.T) {
	config := newTestConfig(t)
	config.ManagedMessengerAppID = "123456"
	config.ManagedMessengerAppSecret = "managed-messenger-app-secret-at-least-32-bytes"
	static, _ := config.accountByKey("messenger-page")
	account := *static
	account.registryManaged = true
	account.PlatformAppID = config.ManagedMessengerAppID

	proofMAC := hmac.New(sha256.New, []byte(config.ManagedMessengerAppSecret))
	_, _ = proofMAC.Write([]byte(account.accessToken))
	wantProof := hex.EncodeToString(proofMAC.Sum(nil))
	fields := "feed"
	debugAppID := config.ManagedMessengerAppID
	debugType := "PAGE"
	debugProfileID := account.ExternalAccountID
	debugUserID := "authority-user"
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v25.0/debug_token":
			if request.Header.Get("Authorization") != "Bearer "+config.ManagedMessengerAppID+"|"+
				config.ManagedMessengerAppSecret {
				t.Errorf("missing exact managed-app access token")
			}
			if request.URL.Query().Get("input_token") != account.accessToken {
				t.Error("debug_token did not inspect the Page access token")
			}
			_, _ = w.Write([]byte(`{"data":{"app_id":"` + debugAppID +
				`","is_valid":true,"profile_id":"` + debugProfileID +
				`","type":"` + debugType + `","user_id":"` + debugUserID + `"}}`))
		case "/v25.0/page-1/subscribed_apps":
			if request.Header.Get("Authorization") != "Bearer "+account.accessToken {
				t.Errorf("missing Page access token")
			}
			if request.URL.Query().Get("appsecret_proof") != wantProof {
				t.Errorf("appsecret_proof = %q", request.URL.Query().Get("appsecret_proof"))
			}
			if request.URL.Query().Get("fields") != "id,subscribed_fields" {
				t.Errorf("subscription fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"123456","subscribed_fields":["` + fields + `"]}]}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer graph.Close()
	server, err := NewServer(config, newMemoryServerStore(), withGraphBases(graph.URL, "http://instagram.invalid"))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected non-messages subscription to fail health")
	}
	fields = "messages"
	if err := server.validateGraphBinding(t.Context(), &account); err != nil {
		t.Fatalf("expected exact messages subscription health: %v", err)
	}
	debugProfileID = ""
	debugUserID = account.ExternalAccountID
	if err := server.validateGraphBinding(t.Context(), &account); err != nil {
		t.Fatalf("expected user_id-only Page-token compatibility health: %v", err)
	}
	debugProfileID = "different-page"
	debugUserID = account.ExternalAccountID
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected Page profile mismatch to override matching user_id and fail health")
	}
	debugProfileID = account.ExternalAccountID
	debugUserID = "authority-user"
	debugType = "USER"
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected non-Page token to fail health")
	}
	debugType = "PAGE"
	debugAppID = "different-app"
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected inspected app mismatch to fail health")
	}
	debugAppID = config.ManagedMessengerAppID
	account.PlatformAppID = "different-app"
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected relay/web platform app mismatch to fail closed")
	}
}

func TestDynamicInstagramHealthPinsManagedAppAndMessagesSubscriptionWithProof(t *testing.T) {
	config := newTestConfig(t)
	config.ManagedInstagramAppID = "789012"
	config.ManagedInstagramAppSecret = "managed-instagram-app-secret-at-least-32-bytes"
	static, _ := config.accountByKey("instagram-direct")
	account := *static
	account.registryManaged = true
	account.PlatformAppID = config.ManagedInstagramAppID

	proofMAC := hmac.New(sha256.New, []byte(config.ManagedInstagramAppSecret))
	_, _ = proofMAC.Write([]byte(account.accessToken))
	wantProof := hex.EncodeToString(proofMAC.Sum(nil))
	fields := "feed"
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+account.accessToken {
			t.Errorf("missing Instagram access token")
		}
		if request.URL.Query().Get("appsecret_proof") != wantProof {
			t.Errorf("appsecret_proof = %q", request.URL.Query().Get("appsecret_proof"))
		}
		switch request.URL.Path {
		case "/v25.0/me":
			if request.URL.Query().Get("fields") != "user_id" {
				t.Errorf("profile fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"data":[{"user_id":"ig-direct-1"}]}`))
		case "/v25.0/ig-direct-1/subscribed_apps":
			_, _ = w.Write([]byte(`{"data":[{"id":"789012","subscribed_fields":["` + fields + `"]}]}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer graph.Close()
	server, err := NewServer(config, newMemoryServerStore(), withGraphBases("http://facebook.invalid", graph.URL))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected non-messages Instagram subscription to fail")
	}
	fields = "messages"
	if err := server.validateGraphBinding(t.Context(), &account); err != nil {
		t.Fatalf("expected exact managed Instagram health: %v", err)
	}
	account.PlatformAppID = "different-app"
	if err := server.validateGraphBinding(t.Context(), &account); err == nil {
		t.Fatal("expected managed Instagram app mismatch to fail closed")
	}
}

func TestAccountHealthValidatesRedisTokenAccountAndGraphHost(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	var facebookCalls atomic.Int32
	var instagramCalls atomic.Int32

	facebook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		facebookCalls.Add(1)
		if request.URL.Path != "/v25.0/me" {
			t.Errorf("Facebook Graph path = %q", request.URL.Path)
		}
		switch request.Header.Get("Authorization") {
		case "Bearer messenger-access-token":
			if request.URL.Query().Get("fields") != "id" {
				t.Errorf("Messenger fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(`{"id":"page-1"}`))
		case "Bearer instagram-page-access-token":
			if request.URL.Query().Get("fields") != "id,instagram_business_account{id}" {
				t.Errorf("Facebook Login Instagram fields = %q", request.URL.Query().Get("fields"))
			}
			_, _ = w.Write([]byte(
				`{"id":"facebook-page-1","instagram_business_account":{"id":"ig-page-1"}}`,
			))
		default:
			http.Error(w, "expired token", http.StatusUnauthorized)
		}
	}))
	defer facebook.Close()

	instagram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		instagramCalls.Add(1)
		if request.URL.Path != "/v25.0/me" {
			t.Errorf("Instagram Graph path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("fields") != "user_id" {
			t.Errorf("Instagram Login fields = %q", request.URL.Query().Get("fields"))
		}
		if request.Header.Get("Authorization") != "Bearer instagram-direct-access-token" {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"user_id":"ig-direct-1"}]}`))
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
	}
	if facebookCalls.Load() != 2 {
		t.Fatalf("Facebook Graph calls = %d, want 2", facebookCalls.Load())
	}
	if instagramCalls.Load() != 1 {
		t.Fatalf("Instagram Graph calls = %d, want 1", instagramCalls.Load())
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
			body:   `{"data":[{"user_id":"some-other-instagram-account"}]}`,
		},
		{name: "obsolete top-level response", status: http.StatusOK, body: `{"user_id":"ig-direct-1"}`},
		{name: "empty data", status: http.StatusOK, body: `{"data":[]}`},
		{name: "multiple profiles", status: http.StatusOK, body: `{"data":[{"user_id":"ig-direct-1"},{"user_id":"ig-direct-1"}]}`},
		{name: "missing professional ID", status: http.StatusOK, body: `{"data":[{}]}`},
		{name: "unknown profile field", status: http.StatusOK, body: `{"data":[{"user_id":"ig-direct-1","id":"app-subject"}]}`},
		{name: "trailing JSON", status: http.StatusOK, body: `{"data":[{"user_id":"ig-direct-1"}]} {}`},
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

func TestAccountHealthRequiresRedisAndDoesNotProbeGraphWhenUnavailable(t *testing.T) {
	config := newTestConfig(t)
	account, _ := config.accountByKey("messenger-page")
	store := newMemoryServerStore()
	store.pingErr = errTestTransport
	var graphCalls atomic.Int32
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"page-1"}`))
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
