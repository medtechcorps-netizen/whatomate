package metarelay

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLoadConfigDefaultsToProductionAndMetaGraphHosts(t *testing.T) {
	config, err := loadTestEnvironment(validConfigEnvironment())
	if err != nil {
		t.Fatalf("load config without Graph overrides: %v", err)
	}
	if config.Environment != "production" ||
		config.FacebookGraphBaseURL != "" || config.InstagramGraphBaseURL != "" {
		t.Fatalf(
			"default environment/overrides = %q/%q/%q",
			config.Environment,
			config.FacebookGraphBaseURL,
			config.InstagramGraphBaseURL,
		)
	}

	var requested []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.Scheme+"://"+request.URL.Host+request.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"recipient_id":"customer-1","message_id":"mid"}`)),
			Request:    request,
		}, nil
	})}
	server, err := NewServer(config, newMemoryServerStore(), WithServerHTTPClient(client))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if server.facebookGraphBase != defaultFacebookGraphBase ||
		server.instagramGraphBase != defaultInstagramGraphBase {
		t.Fatalf("Graph bases = %q/%q", server.facebookGraphBase, server.instagramGraphBase)
	}
	for _, key := range []string{"page", "ig-direct"} {
		account, _ := config.accountByKey(key)
		body := outboundEnvelopeBody(t, account, "default-"+key, "hello")
		if response := performOutbound(t, server.Handler(), account, body); response.Code != http.StatusOK {
			t.Fatalf("%s outbound returned %d (%s)", key, response.Code, response.Body.String())
		}
	}
	want := []string{
		"https://graph.facebook.com/v25.0/page-1/messages",
		"https://graph.instagram.com/v25.0/ig-1/messages",
	}
	if strings.Join(requested, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Graph requests = %q, want %q", requested, want)
	}
}

func TestLoadConfigRefusesGraphBaseOverridesInProduction(t *testing.T) {
	for _, environmentValue := range []string{"", "production", " Production "} {
		for _, variable := range []string{
			"META_RELAY_FACEBOOK_GRAPH_BASE_URL",
			"META_RELAY_INSTAGRAM_GRAPH_BASE_URL",
		} {
			// Restating Meta's own host is still an override.
			for _, value := range []string{
				"https://graph-stub.example.test",
				"https://graph.facebook.com",
				"https://graph.instagram.com",
			} {
				environment := validConfigEnvironment()
				if environmentValue != "" {
					environment["META_RELAY_ENVIRONMENT"] = environmentValue
				}
				environment[variable] = value
				_, err := loadTestEnvironment(environment)
				if err == nil || !strings.Contains(err.Error(), "refused in production") {
					t.Fatalf("environment %q with %s: expected refusal, got %v", environmentValue, variable, err)
				}
				if strings.Contains(err.Error(), value) {
					t.Fatalf("production refusal echoed the configured value: %q", err)
				}
			}
		}
	}
}

func TestLoadConfigRejectsUnknownRelayEnvironment(t *testing.T) {
	for _, value := range []string{"prod", "development", "staging-2", "dev"} {
		environment := validConfigEnvironment()
		environment["META_RELAY_ENVIRONMENT"] = value
		if _, err := loadTestEnvironment(environment); err == nil ||
			!strings.Contains(err.Error(), "META_RELAY_ENVIRONMENT") {
			t.Fatalf("expected unknown environment %q rejection, got %v", value, err)
		}
	}
}

func TestLoadConfigAcceptsBareGraphOriginsOutsideProduction(t *testing.T) {
	for _, environmentValue := range []string{"staging", "local", "test", " Staging "} {
		environment := validConfigEnvironment()
		environment["META_RELAY_ENVIRONMENT"] = environmentValue
		environment["META_RELAY_FACEBOOK_GRAPH_BASE_URL"] = "https://graph-stub.example.test"
		environment["META_RELAY_INSTAGRAM_GRAPH_BASE_URL"] = "http://127.0.0.1:18090/"
		config, err := loadTestEnvironment(environment)
		if err != nil {
			t.Fatalf("environment %q: expected bare origins to load: %v", environmentValue, err)
		}
		if config.Environment != strings.ToLower(strings.TrimSpace(environmentValue)) {
			t.Fatalf("environment = %q", config.Environment)
		}
		server, err := NewServer(config, newMemoryServerStore())
		if err != nil {
			t.Fatalf("environment %q: new server: %v", environmentValue, err)
		}
		if server.facebookGraphBase != "https://graph-stub.example.test" ||
			server.instagramGraphBase != "http://127.0.0.1:18090" {
			t.Fatalf("Graph bases = %q/%q", server.facebookGraphBase, server.instagramGraphBase)
		}
	}

	// One override alone leaves the other product on Meta's host.
	environment := validConfigEnvironment()
	environment["META_RELAY_ENVIRONMENT"] = "staging"
	environment["META_RELAY_INSTAGRAM_GRAPH_BASE_URL"] = "http://[::1]:18090"
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("expected a single IPv6 origin override to load: %v", err)
	}
	server, err := NewServer(config, newMemoryServerStore())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if server.facebookGraphBase != defaultFacebookGraphBase ||
		server.instagramGraphBase != "http://[::1]:18090" {
		t.Fatalf("Graph bases = %q/%q", server.facebookGraphBase, server.instagramGraphBase)
	}

	// A non-production relay without overrides keeps Meta's hosts.
	environment = validConfigEnvironment()
	environment["META_RELAY_ENVIRONMENT"] = "staging"
	if config, err = loadTestEnvironment(environment); err != nil {
		t.Fatalf("expected staging without overrides to load: %v", err)
	}
	if server, err = NewServer(config, newMemoryServerStore()); err != nil {
		t.Fatalf("new server: %v", err)
	}
	if server.facebookGraphBase != defaultFacebookGraphBase ||
		server.instagramGraphBase != defaultInstagramGraphBase {
		t.Fatalf("Graph bases = %q/%q", server.facebookGraphBase, server.instagramGraphBase)
	}
}

func TestLoadConfigAcceptsDialableGraphOriginHosts(t *testing.T) {
	for _, value := range []string{
		"http://graph-stub",
		"http://graph_stub:18090",
		"https://Graph-Stub.example.test",
		"https://graph-stub.example.test:1",
		"https://graph-stub.example.test:65535/",
		"http://127.0.0.1:18090",
		"http://[::1]",
	} {
		environment := validConfigEnvironment()
		environment["META_RELAY_ENVIRONMENT"] = "local"
		environment["META_RELAY_FACEBOOK_GRAPH_BASE_URL"] = value
		config, err := loadTestEnvironment(environment)
		if err != nil {
			t.Fatalf("%q: expected a dialable origin to load: %v", value, err)
		}
		server, err := NewServer(config, newMemoryServerStore())
		if err != nil {
			t.Fatalf("%q: new server: %v", value, err)
		}
		if server.facebookGraphBase != strings.TrimSuffix(value, "/") {
			t.Fatalf("%q: Facebook Graph base = %q", value, server.facebookGraphBase)
		}
	}
}

func TestLoadConfigRejectsGraphBaseThatIsNotABareOrigin(t *testing.T) {
	const secret = "do-not-print-this-secret"
	for _, value := range []string{
		"https://graph-stub.example.test/v25.0",
		"https://graph-stub.example.test/graph/",
		"https://graph-stub.example.test//",
		"https://graph-stub.example.test/%2F",
		"https://graph-stub.example.test?" + secret + "=1",
		"https://graph-stub.example.test/?q=1",
		"https://graph-stub.example.test?",
		"https://graph-stub.example.test#" + secret,
		"https://graph-stub.example.test#",
		"https://user:" + secret + "@graph-stub.example.test",
		"https://" + secret + "@graph-stub.example.test",
		" https://graph-stub.example.test",
		"https://graph-stub.example.test ",
		"ftp://graph-stub.example.test",
		"graph-stub.example.test",
		"//graph-stub.example.test",
		"https:graph-stub.example.test",
		"https://",
		"http://:18090",
		"https://graph stub.example.test",
		// url.Parse accepts these hosts, but the relay could not dial them.
		"https://graph-stub.example.test;x",
		"https://graph-stub.example.test:443:443",
		"https://graph-stub.example.test:",
		"https://graph-stub.example.test:0",
		"https://graph-stub.example.test:65536",
		"https://graph-stub.example.test:99999",
		"https://-graph-stub.example.test",
		"https://graph-stub..example.test",
		"http://[::1]:",
		"http://[fe80::1%25graph-stub]:18090",
		"https://[graph-stub.example.test]",
	} {
		for _, variable := range []string{
			"META_RELAY_FACEBOOK_GRAPH_BASE_URL",
			"META_RELAY_INSTAGRAM_GRAPH_BASE_URL",
		} {
			environment := validConfigEnvironment()
			environment["META_RELAY_ENVIRONMENT"] = "staging"
			environment[variable] = value
			_, err := loadTestEnvironment(environment)
			if err == nil || !strings.Contains(err.Error(), variable) ||
				!strings.Contains(err.Error(), "origin") {
				t.Fatalf("%s=%q: expected origin rejection, got %v", variable, value, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "graph-stub") {
				t.Fatalf("origin rejection echoed the configured value: %q", err)
			}
		}
	}
}

func TestNewServerRefusesGraphBaseOverridesOutsideNonProductionConfig(t *testing.T) {
	// NewServer reads the environment as loadConfig does: trimmed, any case.
	for _, environment := range []string{"", "production", " Production "} {
		config := newTestConfig(t)
		config.Environment = environment
		config.FacebookGraphBaseURL = "https://graph-stub.example.test"
		if _, err := NewServer(config, newMemoryServerStore()); err == nil ||
			!strings.Contains(err.Error(), "refused in production") {
			t.Fatalf("environment %q: expected NewServer refusal, got %v", environment, err)
		}
	}

	config := newTestConfig(t)
	config.Environment = "staging"
	config.InstagramGraphBaseURL = "https://graph-stub.example.test/v25.0"
	if _, err := NewServer(config, newMemoryServerStore()); err == nil ||
		!strings.Contains(err.Error(), "META_RELAY_INSTAGRAM_GRAPH_BASE_URL") {
		t.Fatalf("expected NewServer origin rejection, got %v", err)
	}

	config = newTestConfig(t)
	config.Environment = " Staging "
	config.InstagramGraphBaseURL = "https://graph-stub.example.test"
	server, err := NewServer(config, newMemoryServerStore())
	if err != nil {
		t.Fatalf("expected NewServer to accept a staging override: %v", err)
	}
	if server.instagramGraphBase != "https://graph-stub.example.test" {
		t.Fatalf("Instagram Graph base = %q", server.instagramGraphBase)
	}

	config = newTestConfig(t)
	config.Environment = "unknown"
	if _, err := NewServer(config, newMemoryServerStore()); err == nil ||
		!strings.Contains(err.Error(), "META_RELAY_ENVIRONMENT") {
		t.Fatalf("expected NewServer environment rejection, got %v", err)
	}
}

func TestConfiguredGraphBasesReceiveOutboundDelivery(t *testing.T) {
	var facebookCalls atomic.Int32
	var instagramCalls atomic.Int32
	facebook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		facebookCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v25.0/page-1/messages" {
			t.Errorf("unexpected Facebook Graph request %s %q", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer page-token-value" {
			t.Error("Facebook Graph stub did not receive the Page token")
		}
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"stub-facebook-mid"}`))
	}))
	defer facebook.Close()
	instagram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		instagramCalls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v25.0/ig-1/messages" {
			t.Errorf("unexpected Instagram Graph request %s %q", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer ig-token-value" {
			t.Error("Instagram Graph stub did not receive the profile token")
		}
		_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"stub-instagram-mid"}`))
	}))
	defer instagram.Close()

	environment := validConfigEnvironment()
	environment["META_RELAY_ENVIRONMENT"] = "test"
	environment["META_RELAY_FACEBOOK_GRAPH_BASE_URL"] = facebook.URL
	environment["META_RELAY_INSTAGRAM_GRAPH_BASE_URL"] = instagram.URL
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("load config with Graph stub origins: %v", err)
	}
	// No test-only option: the bases come from the loaded configuration.
	server, err := NewServer(config, newMemoryServerStore())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for key, wantMID := range map[string]string{
		"page":      "stub-facebook-mid",
		"ig-direct": "stub-instagram-mid",
	} {
		account, _ := config.accountByKey(key)
		body := outboundEnvelopeBody(t, account, "stub-"+key, "hello")
		response := performOutbound(t, server.Handler(), account, body)
		if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(wantMID)) {
			t.Fatalf("%s outbound returned %d (%s)", key, response.Code, response.Body.String())
		}
	}
	if facebookCalls.Load() != 1 || instagramCalls.Load() != 1 {
		t.Fatalf(
			"Graph stub calls facebook=%d instagram=%d, want 1/1",
			facebookCalls.Load(),
			instagramCalls.Load(),
		)
	}
}
