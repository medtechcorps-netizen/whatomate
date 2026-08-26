package metarelay

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

type warmRegistryProbe struct {
	cached   *AccountConfig
	current  *AccountConfig
	err      error
	cacheUse []bool
}

func (resolver *warmRegistryProbe) Resolve(
	_ context.Context,
	_ models.Channel,
	_ string,
	_ string,
	useCache bool,
) (*AccountConfig, error) {
	resolver.cacheUse = append(resolver.cacheUse, useCache)
	if useCache && resolver.cached != nil {
		copy := *resolver.cached
		return &copy, nil
	}
	if resolver.err != nil {
		return nil, resolver.err
	}
	copy := *resolver.current
	return &copy, nil
}

func TestSignedBodyVerification(t *testing.T) {
	body := []byte(`{"event":"one"}`)
	signature := signBody("secret-one", body)
	if !verifySignedBody("secret-one", signature, body) {
		t.Fatal("valid signature was rejected")
	}
	uppercaseDigest := signaturePrefix + strings.ToUpper(strings.TrimPrefix(signature, signaturePrefix))
	if !verifySignedBody("secret-one", uppercaseDigest, body) {
		t.Fatal("equivalent uppercase hexadecimal digest was rejected")
	}
	if verifySignedBody("secret-two", signature, body) {
		t.Fatal("cross-secret signature was accepted")
	}
	if verifySignedBody("secret-one", signature, []byte(`{"event":"two"}`)) {
		t.Fatal("tampered body was accepted")
	}
	for _, malformed := range []string{
		"",
		"SHA256=abc",
		"sha256=not-hex",
		"sha256=00",
		signature + "00",
	} {
		if verifySignedBody("secret-one", malformed, body) {
			t.Fatalf("malformed signature %q was accepted", malformed)
		}
	}
}

func TestWebhookRoutesRejectCrossAppCredentials(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler := server.Handler()

	assertVerificationStatus := func(path, token string, want int) {
		t.Helper()
		request := httptest.NewRequest(
			http.MethodGet,
			path+"?hub.mode=subscribe&hub.challenge=challenge&hub.verify_token="+token,
			nil,
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s with token %q: got %d, want %d", path, token, response.Code, want)
		}
	}

	assertVerificationStatus(
		"/v1/meta/messenger/webhook",
		config.MessengerVerifyToken,
		http.StatusOK,
	)
	assertVerificationStatus(
		"/v1/meta/messenger/webhook",
		config.InstagramLoginVerifyToken,
		http.StatusForbidden,
	)
	assertVerificationStatus(
		"/v1/meta/instagram/webhook",
		config.InstagramLoginVerifyToken,
		http.StatusOK,
	)
	assertVerificationStatus(
		"/v1/meta/instagram/webhook",
		config.MessengerVerifyToken,
		http.StatusForbidden,
	)

	pageBody := []byte(`{"object":"page","entry":[{"id":"page-1","time":1770000000,"messaging":[]}]}`)
	instagramBody := []byte(`{"object":"instagram","entry":[{"id":"ig-direct-1","time":1770000000,"messaging":[]}]}`)
	assertWebhookStatus := func(path, secret string, body []byte, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set(MetaSignatureHeader, signBody(secret, body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: got %d (%s), want %d", path, response.Code, response.Body.String(), want)
		}
	}

	assertWebhookStatus(
		"/v1/meta/messenger/webhook",
		config.InstagramLoginAppSecret,
		pageBody,
		http.StatusUnauthorized,
	)
	assertWebhookStatus(
		"/v1/meta/instagram/webhook",
		config.MessengerAppSecret,
		instagramBody,
		http.StatusUnauthorized,
	)
	assertWebhookStatus(
		"/v1/meta/messenger/webhook",
		config.MessengerAppSecret,
		pageBody,
		http.StatusOK,
	)
	assertWebhookStatus(
		"/v1/meta/instagram/webhook",
		config.InstagramLoginAppSecret,
		instagramBody,
		http.StatusOK,
	)

	// Even a correctly signed delivery cannot cross app/account bindings.
	assertWebhookStatus(
		"/v1/meta/messenger/webhook",
		config.MessengerAppSecret,
		instagramBody,
		http.StatusNotFound,
	)
	assertWebhookStatus(
		"/v1/meta/instagram/webhook",
		config.InstagramLoginAppSecret,
		pageBody,
		http.StatusNotFound,
	)

	oldRoute := httptest.NewRequest(http.MethodPost, "/v1/meta/webhook", bytes.NewReader(pageBody))
	oldRoute.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, pageBody))
	oldResponse := httptest.NewRecorder()
	handler.ServeHTTP(oldResponse, oldRoute)
	if oldResponse.Code != http.StatusNotFound {
		t.Fatalf("legacy shared webhook route returned %d, want 404", oldResponse.Code)
	}
	if store.acceptCalls != 2 {
		t.Fatalf("durable accept called %d times, want 2", store.acceptCalls)
	}
}

func TestManagedMessengerWebhookUsesDistinctDynamicCredentialsAndKeepsStaticRouteCompatible(t *testing.T) {
	config := newTestConfig(t)
	config.RegistryEnabled = true
	config.RegistryURL = "https://app.example.test/internal/meta-registry/v1/resolve"
	config.RegistrySecret = "registry-service-secret-at-least-32-bytes"
	config.RegistryEdgeSecret = "registry-edge-secret-at-least-32-bytes"
	config.ManagedMessengerAppID = "123456"
	config.ManagedMessengerAppSecret = "managed-messenger-app-secret-at-least-32-bytes"
	config.ManagedMessengerVerifyToken = "managed-messenger-verify-token"
	static, _ := config.accountByKey("messenger-page")
	dynamic := *static
	dynamic.Key = "managed-page"
	dynamic.ExternalAccountID = "managed-page-1"
	dynamic.PlatformAppID = config.ManagedMessengerAppID
	dynamic.registryManaged = true
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.registry = fixedRegistryResolver{account: &dynamic}
	handler := server.Handler()

	assertVerification := func(path, token string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet,
			path+"?hub.mode=subscribe&hub.challenge=challenge&hub.verify_token="+token, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s verification returned %d (%s), want %d", path, response.Code, response.Body.String(), want)
		}
	}
	assertVerification("/v1/meta/messenger/managed-webhook", config.ManagedMessengerVerifyToken, http.StatusOK)
	assertVerification("/v1/meta/messenger/managed-webhook", config.MessengerVerifyToken, http.StatusForbidden)
	assertVerification("/v1/meta/messenger/webhook", config.ManagedMessengerVerifyToken, http.StatusForbidden)

	managedBody := []byte(`{"object":"page","entry":[{"id":"managed-page-1","time":1770000000,"messaging":[]}]}`)
	staticBody := []byte(`{"object":"page","entry":[{"id":"page-1","time":1770000000,"messaging":[]}]}`)
	assertWebhook := func(path, secret string, body []byte, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set(MetaSignatureHeader, signBody(secret, body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s returned %d (%s), want %d", path, response.Code, response.Body.String(), want)
		}
	}
	assertWebhook("/v1/meta/messenger/managed-webhook", config.MessengerAppSecret, managedBody, http.StatusUnauthorized)
	assertWebhook("/v1/meta/messenger/webhook", config.ManagedMessengerAppSecret, staticBody, http.StatusUnauthorized)
	assertWebhook("/v1/meta/messenger/managed-webhook", config.ManagedMessengerAppSecret, staticBody, http.StatusNotFound)
	assertWebhook("/v1/meta/messenger/managed-webhook", config.ManagedMessengerAppSecret, managedBody, http.StatusOK)
	assertWebhook("/v1/meta/messenger/webhook", config.MessengerAppSecret, staticBody, http.StatusOK)
	if store.acceptCalls != 2 {
		t.Fatalf("durable accept called %d times, want managed+legacy only", store.acceptCalls)
	}
}

func TestManagedInstagramWebhookIsIsolatedAndStaticMappingKeepsPrecedence(t *testing.T) {
	config := newTestConfig(t)
	config.RegistryEnabled = true
	config.RegistryURL = "https://app.example.test/internal/meta-registry/v1/resolve"
	config.RegistrySecret = "registry-service-secret-at-least-32-bytes"
	config.RegistryEdgeSecret = "registry-edge-secret-at-least-32-bytes"
	config.ManagedInstagramAppID = "789012"
	config.ManagedInstagramAppSecret = "managed-instagram-app-secret-at-least-32-bytes"
	config.ManagedInstagramVerifyToken = "managed-instagram-verify-token"
	static, _ := config.accountByKey("instagram-direct")
	dynamic := *static
	dynamic.Key = "managed-instagram-profile"
	dynamic.ExternalAccountID = "managed-ig-1"
	dynamic.PlatformAppID = config.ManagedInstagramAppID
	dynamic.registryManaged = true
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.registry = fixedRegistryResolver{account: &dynamic}
	handler := server.Handler()

	verify := func(path, token string, want int) {
		t.Helper()
		request := httptest.NewRequest(
			http.MethodGet,
			path+"?hub.mode=subscribe&hub.challenge=challenge&hub.verify_token="+token,
			nil,
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s verification returned %d, want %d", path, response.Code, want)
		}
	}
	verify("/v1/meta/instagram/managed-webhook", config.ManagedInstagramVerifyToken, http.StatusOK)
	verify("/v1/meta/instagram/managed-webhook", config.InstagramLoginVerifyToken, http.StatusForbidden)
	verify("/v1/meta/instagram/webhook", config.ManagedInstagramVerifyToken, http.StatusForbidden)

	managedBody := []byte(`{"object":"instagram","entry":[{"id":"managed-ig-1","time":1770000000,"messaging":[{"sender":{"id":"customer-1"},"recipient":{"id":"managed-ig-1"},"timestamp":1770000000000,"message":{"mid":"managed-mid","text":"hello"}}]}]}`)
	staticBody := []byte(`{"object":"instagram","entry":[{"id":"ig-direct-1","time":1770000000,"messaging":[{"sender":{"id":"customer-2"},"recipient":{"id":"ig-direct-1"},"timestamp":1770000000000,"message":{"mid":"static-mid","text":"hello"}}]}]}`)
	post := func(path, secret string, body []byte, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set(MetaSignatureHeader, signBody(secret, body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s returned %d (%s), want %d", path, response.Code, response.Body.String(), want)
		}
	}
	post("/v1/meta/instagram/managed-webhook", config.InstagramLoginAppSecret, managedBody, http.StatusUnauthorized)
	post("/v1/meta/instagram/webhook", config.ManagedInstagramAppSecret, staticBody, http.StatusUnauthorized)
	post("/v1/meta/instagram/managed-webhook", config.ManagedInstagramAppSecret, staticBody, http.StatusNotFound)
	post("/v1/meta/instagram/managed-webhook", config.ManagedInstagramAppSecret, managedBody, http.StatusOK)
	post("/v1/meta/instagram/webhook", config.InstagramLoginAppSecret, staticBody, http.StatusOK)
	if store.acceptCalls != 2 {
		t.Fatalf("durable accept called %d times, want managed+static only", store.acceptCalls)
	}
}

func TestManagedInstagramInboundBypassesWarmRegistryLeaseAfterDowngrade(t *testing.T) {
	config := newTestConfig(t)
	config.RegistryEnabled = true
	config.RegistryURL = "https://app.example.test/internal/meta-registry/v1/resolve"
	config.RegistrySecret = "registry-service-secret-at-least-32-bytes"
	config.RegistryEdgeSecret = "registry-edge-secret-at-least-32-bytes"
	config.ManagedInstagramAppID = "789012"
	config.ManagedInstagramAppSecret = "managed-instagram-app-secret-at-least-32-bytes"
	config.ManagedInstagramVerifyToken = "managed-instagram-verify-token"
	static, _ := config.accountByKey("instagram-direct")
	dynamic := *static
	dynamic.Key = "managed-instagram-cache-probe"
	dynamic.ExternalAccountID = "managed-ig-cache-probe"
	dynamic.PlatformAppID = config.ManagedInstagramAppID
	dynamic.registryManaged = true
	resolver := &warmRegistryProbe{
		cached: &dynamic,
		err:    ErrRegistryStale,
	}
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.registry = resolver
	body := []byte(`{"object":"instagram","entry":[{"id":"managed-ig-cache-probe","time":1770000000,"messaging":[]}]}`)
	request := httptest.NewRequest(
		http.MethodPost, "/v1/meta/instagram/managed-webhook", bytes.NewReader(body),
	)
	request.Header.Set(MetaSignatureHeader, signBody(config.ManagedInstagramAppSecret, body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("managed webhook returned %d (%s), want 503", response.Code, response.Body.String())
	}
	if len(resolver.cacheUse) != 1 || resolver.cacheUse[0] {
		t.Fatalf("managed Instagram inbound used cached registry lease: %#v", resolver.cacheUse)
	}
	if store.acceptCalls != 0 {
		t.Fatalf("durable accept called %d times after downgrade, want zero", store.acceptCalls)
	}
}

func TestWebhookAcceptanceIsIdempotent(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := []byte(`{
	  "object":"page",
	  "entry":[{
	    "id":"page-1",
	    "time":1770000000,
	    "messaging":[{
	      "sender":{"id":"customer-1"},
	      "recipient":{"id":"page-1"},
	      "timestamp":1770000000123,
	      "message":{"mid":"mid-1","text":"hello"}
	    }]
	  }]
	}`)
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/meta/messenger/webhook",
			bytes.NewReader(body),
		)
		request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, body))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d: got %d (%s)", attempt, response.Code, response.Body.String())
		}
	}
	if len(store.accepted) != 1 || store.acceptCalls != 2 {
		t.Fatalf("accepted=%d calls=%d, want one durable acceptance across two calls", len(store.accepted), store.acceptCalls)
	}
}

func TestWebhookAcknowledgesOnlyAfterAllSplitJobsAreDurablyAccepted(t *testing.T) {
	config := newTestConfig(t)
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	raw := testMetaTextWebhookBody(t, 6000, "hello")
	if len(raw) > int(defaultInboundBodyLimit) {
		t.Fatalf(
			"test raw body is %d bytes and exceeds the webhook limit %d",
			len(raw),
			defaultInboundBodyLimit,
		)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/meta/messenger/webhook",
		bytes.NewReader(raw),
	)
	request.Header.Set(MetaSignatureHeader, signBody(config.MessengerAppSecret, raw))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("got %d (%s), want acknowledged webhook", response.Code, response.Body.String())
	}
	jobs := store.accepted[digestHex(raw)]
	if store.acceptCalls != 1 || len(jobs) <= 1 {
		t.Fatalf(
			"durable acceptance calls=%d jobs=%d, want one atomic multi-job acceptance",
			store.acceptCalls,
			len(jobs),
		)
	}
	for index, job := range jobs {
		if len(job.Body) > channelapi.RelayCanonicalWebhookMaxBodyBytes {
			t.Fatalf(
				"accepted job %d has %d bytes, canonical maximum is %d",
				index,
				len(job.Body),
				channelapi.RelayCanonicalWebhookMaxBodyBytes,
			)
		}
	}
}
