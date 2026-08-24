package metarelay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	testStaticMessengerAppID     = "1035383549213572"
	testStaticMessengerAppSecret = "0123456789abcdef0123456789abcdef"
	testStaticMessengerVerify    = "static-messenger-verify-token-0123456789abcdef"
	testStaticMessengerSecretEnv = "TEST_STATIC_MESSENGER_APP_SECRET"
	testStaticMessengerVerifyEnv = "TEST_STATIC_MESSENGER_VERIFY_TOKEN"
)

func staticMessengerEnvironment() map[string]string {
	environment := validConfigEnvironment()
	environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = `[{` +
		`"app_id":"` + testStaticMessengerAppID + `",` +
		`"app_secret_env":"` + testStaticMessengerSecretEnv + `",` +
		`"verify_token_env":"` + testStaticMessengerVerifyEnv + `"` +
		`}]`
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
		validAccountsJSON,
		`"channel": "messenger",`,
		`"channel": "messenger", "messenger_app_id": "`+testStaticMessengerAppID+`",`,
		1,
	)
	environment[testStaticMessengerSecretEnv] = testStaticMessengerAppSecret
	environment[testStaticMessengerVerifyEnv] = testStaticMessengerVerify
	return environment
}

func newStaticMessengerTestConfig(t *testing.T) *Config {
	t.Helper()
	config, err := loadTestEnvironment(staticMessengerEnvironment())
	if err != nil {
		t.Fatalf("load static Messenger test config: %v", err)
	}
	return config
}

func newStaticMessengerTestConfigWithLegacyPage(t *testing.T) *Config {
	t.Helper()
	environment := staticMessengerEnvironment()
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.TrimSuffix(
		environment["META_RELAY_ACCOUNTS_JSON"], "]",
	) + `,{` +
		`"key":"legacy-page",` +
		`"channel":"messenger",` +
		`"external_account_id":"legacy-page-2",` +
		`"rereply_webhook_url":"https://rereply.example.test/meta",` +
		`"access_token_env":"LEGACY_PAGE_TOKEN",` +
		`"rereply_inbound_secret_env":"LEGACY_PAGE_INBOUND",` +
		`"rereply_outbound_secret_env":"LEGACY_PAGE_OUTBOUND"` +
		`}]`
	environment["LEGACY_PAGE_TOKEN"] = "legacy-page-token"
	environment["LEGACY_PAGE_INBOUND"] = "legacy-page-inbound"
	environment["LEGACY_PAGE_OUTBOUND"] = "legacy-page-outbound"
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("load mixed static Messenger test config: %v", err)
	}
	return config
}

func TestLoadConfigBindsAdditionalStaticMessengerAppWithoutChangingLegacyDefaults(t *testing.T) {
	config := newStaticMessengerTestConfig(t)
	app, ok := config.staticMessengerApp(testStaticMessengerAppID)
	if !ok || app.appSecret != testStaticMessengerAppSecret ||
		app.verifyToken != testStaticMessengerVerify {
		t.Fatal("additional static Messenger application was not indexed")
	}
	account, ok := config.account(models.ChannelMessenger, "page-1")
	if !ok || account.MessengerAppID != testStaticMessengerAppID ||
		account.PlatformAppID != testStaticMessengerAppID ||
		account.webhookApp() != staticMessengerWebhookApp(testStaticMessengerAppID) {
		t.Fatalf("Messenger account was not bound to the additional app: %#v", account)
	}

	legacy, err := loadTestEnvironment(validConfigEnvironment())
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	legacyAccount, _ := legacy.account(models.ChannelMessenger, "page-1")
	if len(legacy.StaticMessengerApps) != 0 || legacyAccount.webhookApp() != WebhookAppMessenger {
		t.Fatal("optional additional-app configuration changed the legacy route")
	}

	routeFirstEnvironment := staticMessengerEnvironment()
	routeFirstEnvironment["META_RELAY_ACCOUNTS_JSON"] = validAccountsJSON
	routeFirst, err := loadTestEnvironment(routeFirstEnvironment)
	if err != nil {
		t.Fatalf("load unbound route-first config: %v", err)
	}
	routeFirstAccount, _ := routeFirst.account(models.ChannelMessenger, "page-1")
	if _, ok := routeFirst.staticMessengerApp(testStaticMessengerAppID); !ok ||
		routeFirstAccount.webhookApp() != WebhookAppMessenger {
		t.Fatal("unbound additional route changed a legacy account")
	}
}

func TestLoadConfigRejectsUnsafeStaticMessengerAppBindingsAndCredentials(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "unknown account app",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
					environment["META_RELAY_ACCOUNTS_JSON"], testStaticMessengerAppID, "999999", 1,
				)
			},
			want: "does not identify",
		},
		{
			name: "app binding on Instagram",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_ACCOUNTS_JSON"] = strings.Replace(
					environment["META_RELAY_ACCOUNTS_JSON"],
					`"channel": "instagram",`,
					`"channel": "instagram", "messenger_app_id": "`+testStaticMessengerAppID+`",`,
					1,
				)
			},
			want: "only valid for Messenger",
		},
		{
			name: "noncanonical app ID",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.Replace(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
					testStaticMessengerAppID, "01", 1,
				)
			},
			want: "canonical nonzero numeric",
		},
		{
			name: "duplicate app ID",
			mutate: func(environment map[string]string) {
				environment["SECOND_STATIC_SECRET"] = strings.Repeat("s", 32)
				environment["SECOND_STATIC_VERIFY"] = strings.Repeat("v", 32)
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.TrimSuffix(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"], "]",
				) + `,{"app_id":"` + testStaticMessengerAppID +
					`","app_secret_env":"SECOND_STATIC_SECRET","verify_token_env":"SECOND_STATIC_VERIFY"}]`
			},
			want: "duplicate static Messenger app_id",
		},
		{
			name: "managed app ID collision",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_REGISTRY_ENABLED"] = "true"
				environment["META_RELAY_DYNAMIC_QUEUE_READER_VERSION"] = "2"
				environment["META_RELAY_REGISTRY_URL"] = "https://app.example.test/internal/meta-registry/v1/resolve"
				environment["META_RELAY_REGISTRY_SECRET"] = strings.Repeat("r", 32)
				environment["META_RELAY_REGISTRY_EDGE_SECRET"] = strings.Repeat("e", 32)
				environment["META_RELAY_MANAGED_MESSENGER_APP_ID"] = testStaticMessengerAppID
				environment["META_RELAY_MANAGED_MESSENGER_APP_SECRET"] = strings.Repeat("m", 32)
				environment["META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN"] = strings.Repeat("t", 32)
			},
			want: "conflicts with a managed Meta application",
		},
		{
			name: "reused credential environment",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.Replace(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
					testStaticMessengerVerifyEnv, testStaticMessengerSecretEnv, 1,
				)
			},
			want: "reuses a credential environment",
		},
		{
			name: "account credential environment reused",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.Replace(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
					testStaticMessengerSecretEnv, "PAGE_TOKEN", 1,
				)
			},
			want: "dedicated credential environment variables",
		},
		{
			name: "relay configuration variable used as credential",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.Replace(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
					testStaticMessengerSecretEnv,
					"META_RELAY_STATIC_MESSENGER_APPS_JSON",
					1,
				)
			},
			want: "outside the META_RELAY_ namespace",
		},
		{
			name: "missing secret",
			mutate: func(environment map[string]string) {
				delete(environment, testStaticMessengerSecretEnv)
			},
			want: "app secret must contain",
		},
		{
			name: "short verify token",
			mutate: func(environment map[string]string) {
				environment[testStaticMessengerVerifyEnv] = "short"
			},
			want: "verify token must contain",
		},
		{
			name: "credential surrounding whitespace",
			mutate: func(environment map[string]string) {
				environment[testStaticMessengerSecretEnv] = " " + testStaticMessengerAppSecret
			},
			want: "app secret must contain",
		},
		{
			name: "credential reused across Meta apps",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_MESSENGER_APP_SECRET"] = testStaticMessengerAppSecret
			},
			want: "unique across configured Meta applications",
		},
		{
			name: "secret equals verify token",
			mutate: func(environment map[string]string) {
				environment[testStaticMessengerVerifyEnv] = testStaticMessengerAppSecret
			},
			want: "unique across configured Meta applications",
		},
		{
			name: "unknown descriptor field",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.Replace(
					environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
					`"app_id":`, `"unexpected":true,"app_id":`, 1,
				)
			},
			want: "is invalid",
		},
		{
			name: "null descriptor inventory",
			mutate: func(environment map[string]string) {
				environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = "null"
			},
			want: "is invalid",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := staticMessengerEnvironment()
			test.mutate(environment)
			_, err := loadTestEnvironment(environment)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), testStaticMessengerAppSecret) ||
				strings.Contains(err.Error(), testStaticMessengerVerify) {
				t.Fatalf("configuration error exposed a credential: %q", err)
			}
		})
	}
}

type countingRegistryResolver struct {
	calls atomic.Int32
}

func (resolver *countingRegistryResolver) Resolve(
	context.Context,
	models.Channel,
	string,
	string,
	bool,
) (*AccountConfig, error) {
	resolver.calls.Add(1)
	return nil, ErrRegistryNotFound
}

func TestAdditionalStaticMessengerRouteIsExactAndAppIsolated(t *testing.T) {
	config := newStaticMessengerTestConfigWithLegacyPage(t)
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	registry := &countingRegistryResolver{}
	server.registry = registry
	handler := server.Handler()
	route := "/v1/meta/messenger/apps/" + testStaticMessengerAppID + "/webhook"

	verify := func(rawURL string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, rawURL, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("GET %s = %d (%s), want %d", rawURL, response.Code, response.Body.String(), want)
		}
	}
	query := "?hub.mode=subscribe&hub.challenge=challenge&hub.verify_token=" +
		url.QueryEscape(testStaticMessengerVerify)
	verify(route+query, http.StatusOK)
	verify(route+strings.Replace(query, testStaticMessengerVerify, "wrong-token", 1), http.StatusForbidden)
	verify(route+query+"&hub.verify_token="+url.QueryEscape(testStaticMessengerVerify), http.StatusForbidden)
	verify("/v1/meta/messenger/apps/999999/webhook"+query, http.StatusNotFound)

	body := []byte(`{"object":"page","entry":[{"id":"page-1","time":1770000000,"messaging":[{"sender":{"id":"customer-1"},"recipient":{"id":"page-1"},"timestamp":1770000000000,"message":{"mid":"static-app-mid","text":"hello"}}]}]}`)
	post := func(path, secret string, headers []string, body []byte, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		if len(headers) == 0 {
			request.Header.Set(MetaSignatureHeader, signBody(secret, body))
		} else {
			for _, signature := range headers {
				request.Header.Add(MetaSignatureHeader, signature)
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("POST %s = %d (%s), want %d", path, response.Code, response.Body.String(), want)
		}
	}
	post(route, testStaticMessengerAppSecret, nil, body, http.StatusOK)
	accepted := store.accepted[inboundAcceptanceID(
		staticMessengerWebhookApp(testStaticMessengerAppID),
		body,
	)]
	if len(accepted) != 1 ||
		accepted[0].SchemaVersion != staticMessengerInboundJobSchemaVersion ||
		accepted[0].WebhookApp != staticMessengerWebhookApp(testStaticMessengerAppID) {
		t.Fatalf("accepted app-bound jobs = %#v", accepted)
	}
	legacyBody := []byte(`{"object":"page","entry":[{"id":"legacy-page-2","time":1770000000,"messaging":[]}]}`)
	post("/v1/meta/messenger/webhook", config.MessengerAppSecret, nil, legacyBody, http.StatusOK)
	post(route, testStaticMessengerAppSecret, nil, legacyBody, http.StatusNotFound)
	acceptedCalls := store.acceptCalls

	post(route, config.MessengerAppSecret, nil, body, http.StatusUnauthorized)
	post("/v1/meta/messenger/webhook", testStaticMessengerAppSecret, nil, body, http.StatusUnauthorized)
	post("/v1/meta/messenger/webhook", config.MessengerAppSecret, nil, body, http.StatusNotFound)
	unknown := []byte(`{"object":"page","entry":[{"id":"unknown-page","time":1770000000,"messaging":[]}]}`)
	post(route, testStaticMessengerAppSecret, nil, unknown, http.StatusNotFound)
	mixed := []byte(`{"object":"page","entry":[{"id":"page-1","time":1770000000,"messaging":[]},{"id":"unknown-page","time":1770000000,"messaging":[]}]}`)
	post(route, testStaticMessengerAppSecret, nil, mixed, http.StatusNotFound)
	post(route, "", []string{signBody(testStaticMessengerAppSecret, body), signBody(testStaticMessengerAppSecret, body)}, body, http.StatusUnauthorized)
	post(route, "", []string{signBody(testStaticMessengerAppSecret, body)}, append(append([]byte(nil), body...), ' '), http.StatusUnauthorized)

	post(route+"/", testStaticMessengerAppSecret, nil, body, http.StatusNotFound)
	post("/v1/meta//messenger/apps/"+testStaticMessengerAppID+"/webhook", testStaticMessengerAppSecret, nil, body, http.StatusNotFound)
	post("/v1/meta/messenger/apps/%31"+testStaticMessengerAppID[1:]+"/webhook", testStaticMessengerAppSecret, nil, body, http.StatusNotFound)
	if store.acceptCalls != acceptedCalls {
		t.Fatalf("rejected requests reached Redis: calls=%d, want %d", store.acceptCalls, acceptedCalls)
	}
	if registry.calls.Load() != 0 {
		t.Fatalf("additional static route fell through to registry %d times", registry.calls.Load())
	}
}

func TestTwoAdditionalStaticMessengerAppsRemainMutuallyIsolated(t *testing.T) {
	const (
		secondAppID        = "2000000000000001"
		secondSecretEnv    = "SECOND_MESSENGER_APP_SECRET"
		secondVerifyEnv    = "SECOND_MESSENGER_VERIFY_TOKEN"
		secondAppSecret    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		secondVerifyToken  = "cccccccccccccccccccccccccccccccc"
		secondPageTokenEnv = "SECOND_PAGE_TOKEN"
	)
	environment := staticMessengerEnvironment()
	environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"] = strings.TrimSuffix(
		environment["META_RELAY_STATIC_MESSENGER_APPS_JSON"],
		"]",
	) + `,{"app_id":"` + secondAppID + `","app_secret_env":"` + secondSecretEnv +
		`","verify_token_env":"` + secondVerifyEnv + `"}]`
	environment["META_RELAY_ACCOUNTS_JSON"] = strings.TrimSuffix(
		environment["META_RELAY_ACCOUNTS_JSON"],
		"]",
	) + `,{"key":"page-two","channel":"messenger","external_account_id":"page-2",` +
		`"messenger_app_id":"` + secondAppID + `",` +
		`"rereply_webhook_url":"https://rereply.example.test/meta",` +
		`"access_token_env":"` + secondPageTokenEnv + `",` +
		`"rereply_inbound_secret_env":"SECOND_PAGE_INBOUND",` +
		`"rereply_outbound_secret_env":"SECOND_PAGE_OUTBOUND"}]`
	environment[secondSecretEnv] = secondAppSecret
	environment[secondVerifyEnv] = secondVerifyToken
	environment[secondPageTokenEnv] = "second-page-token"
	environment["SECOND_PAGE_INBOUND"] = "second-page-inbound"
	environment["SECOND_PAGE_OUTBOUND"] = "second-page-outbound"
	config, err := loadTestEnvironment(environment)
	if err != nil {
		t.Fatalf("load two-app config: %v", err)
	}
	store := newMemoryServerStore()
	server, err := NewServer(config, store)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	registry := &countingRegistryResolver{}
	server.registry = registry
	handler := server.Handler()
	firstRoute := "/v1/meta/messenger/apps/" + testStaticMessengerAppID + "/webhook"
	secondRoute := "/v1/meta/messenger/apps/" + secondAppID + "/webhook"

	verify := func(route, token string, want int) {
		t.Helper()
		request := httptest.NewRequest(
			http.MethodGet,
			route+"?hub.mode=subscribe&hub.challenge=challenge&hub.verify_token="+
				url.QueryEscape(token),
			nil,
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("verify %s = %d (%s), want %d", route, response.Code, response.Body.String(), want)
		}
	}
	verify(firstRoute, testStaticMessengerVerify, http.StatusOK)
	verify(secondRoute, secondVerifyToken, http.StatusOK)
	verify(firstRoute, secondVerifyToken, http.StatusForbidden)
	verify(secondRoute, testStaticMessengerVerify, http.StatusForbidden)

	firstBody := []byte(`{"object":"page","entry":[{"id":"page-1","time":1770000000,"messaging":[]}]}`)
	secondBody := []byte(`{"object":"page","entry":[{"id":"page-2","time":1770000000,"messaging":[]}]}`)
	post := func(route, secret string, body []byte, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
		request.Header.Set(MetaSignatureHeader, signBody(secret, body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("POST %s = %d (%s), want %d", route, response.Code, response.Body.String(), want)
		}
	}
	post(firstRoute, testStaticMessengerAppSecret, firstBody, http.StatusOK)
	post(secondRoute, secondAppSecret, secondBody, http.StatusOK)
	post(secondRoute, testStaticMessengerAppSecret, firstBody, http.StatusUnauthorized)
	post(secondRoute, secondAppSecret, firstBody, http.StatusNotFound)
	post(firstRoute, secondAppSecret, secondBody, http.StatusUnauthorized)
	post(firstRoute, testStaticMessengerAppSecret, secondBody, http.StatusNotFound)

	if store.acceptCalls != 2 || len(store.accepted) != 2 {
		t.Fatalf("accepted calls=%d markers=%d, want two", store.acceptCalls, len(store.accepted))
	}
	if registry.calls.Load() != 0 {
		t.Fatalf("two-app static routes fell through to registry %d times", registry.calls.Load())
	}
}

func TestAdditionalStaticMessengerHealthAndOutboundPinExactApp(t *testing.T) {
	config := newStaticMessengerTestConfig(t)
	account, _ := config.accountByKey("page")
	wantProof := metaAccessTokenProof(account.accessToken, testStaticMessengerAppSecret)
	debugAppID := testStaticMessengerAppID
	debugPageID := account.ExternalAccountID
	subscribedFields := "messages"
	var outboundCalls atomic.Int32

	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v25.0/debug_token":
			if request.Header.Get("Authorization") != "Bearer "+testStaticMessengerAppID+"|"+
				testStaticMessengerAppSecret {
				t.Errorf("debug_token did not use the exact additional app access token")
			}
			if request.URL.Query().Get("input_token") != account.accessToken {
				t.Error("debug_token did not inspect the exact Page token")
			}
			_, _ = w.Write([]byte(`{"data":{"app_id":"` + debugAppID +
				`","is_valid":true,"profile_id":"` + debugPageID + `","type":"PAGE"}}`))
		case "/v25.0/page-1/subscribed_apps":
			if request.Header.Get("Authorization") != "Bearer "+account.accessToken ||
				request.URL.Query().Get("appsecret_proof") != wantProof {
				t.Error("subscription health did not use the exact Page token and proof")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"` + testStaticMessengerAppID +
				`","subscribed_fields":["` + subscribedFields + `"]}]}`))
		case "/v25.0/page-1/messages":
			outboundCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer "+account.accessToken ||
				request.URL.Query().Get("appsecret_proof") != wantProof {
				t.Error("outbound did not use the exact Page token and proof")
			}
			if strings.Contains(request.URL.String(), account.accessToken) ||
				strings.Contains(request.URL.String(), testStaticMessengerAppSecret) {
				t.Error("outbound URL exposed a credential")
			}
			_, _ = w.Write([]byte(`{"recipient_id":"customer-1","message_id":"static-app-outbound-mid"}`))
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
	if err := server.validateGraphBinding(t.Context(), account); err != nil {
		t.Fatalf("exact additional app health failed: %v", err)
	}
	subscribedFields = "feed"
	if err := server.validateGraphBinding(t.Context(), account); err == nil {
		t.Fatal("non-messages subscription passed health")
	}
	subscribedFields = "messages"
	debugAppID = "999999"
	if err := server.validateGraphBinding(t.Context(), account); err == nil {
		t.Fatal("wrong token app passed health")
	}
	debugAppID = testStaticMessengerAppID
	debugPageID = "different-page"
	if err := server.validateGraphBinding(t.Context(), account); err == nil {
		t.Fatal("wrong token Page passed health")
	}
	debugPageID = account.ExternalAccountID

	result := server.sendGraph(t.Context(), account, channelapi.OutboundMessage{
		Recipient: channelapi.Participant{ExternalID: "customer-1"},
		Parts:     []channelapi.MessagePart{{Type: models.MessagePartTypeText, Text: "hello"}},
	})
	if result.status != http.StatusOK || outboundCalls.Load() != 1 {
		t.Fatalf("outbound status=%d calls=%d body=%s", result.status, outboundCalls.Load(), result.body)
	}
}

func TestWorkerRejectsQueuedAdditionalAppJobAfterAccountRebind(t *testing.T) {
	for _, test := range []struct {
		name              string
		schemaVersion     int
		webhookApp        WebhookApp
		channel           models.Channel
		externalAccountID string
		wantReason        string
	}{
		{
			name:              "different additional app",
			schemaVersion:     staticMessengerInboundJobSchemaVersion,
			webhookApp:        staticMessengerWebhookApp("999999"),
			channel:           models.ChannelMessenger,
			externalAccountID: "page-1",
			wantReason:        "webhook_app_binding_changed",
		},
		{
			name:              "legacy job without app fence",
			schemaVersion:     InboundJobSchemaVersion,
			channel:           models.ChannelMessenger,
			externalAccountID: "page-1",
			wantReason:        "webhook_app_binding_changed",
		},
		{
			name:              "version three job without app fence",
			schemaVersion:     staticMessengerInboundJobSchemaVersion,
			channel:           models.ChannelMessenger,
			externalAccountID: "page-1",
			wantReason:        "invalid_static_messenger_job",
		},
		{
			name:              "version two job carrying an app fence",
			schemaVersion:     InboundJobSchemaVersion,
			webhookApp:        staticMessengerWebhookApp(testStaticMessengerAppID),
			channel:           models.ChannelMessenger,
			externalAccountID: "page-1",
			wantReason:        "invalid_static_messenger_job",
		},
		{
			name:              "same app rebound to a different Page",
			schemaVersion:     staticMessengerInboundJobSchemaVersion,
			webhookApp:        staticMessengerWebhookApp(testStaticMessengerAppID),
			channel:           models.ChannelMessenger,
			externalAccountID: "old-page",
			wantReason:        "static_account_binding_changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := newStaticMessengerTestConfig(t)
			store := &fakeQueueStore{jobs: []InboundJob{{
				SchemaVersion:     test.schemaVersion,
				ID:                "app-fenced-job",
				AccountKey:        "page",
				Channel:           test.channel,
				ExternalAccountID: test.externalAccountID,
				WebhookApp:        test.webhookApp,
				Body:              []byte(`{"external_account_id":"page-1","events":[]}`),
			}}}
			worker, err := NewWorker(config, store)
			if err != nil {
				t.Fatalf("new worker: %v", err)
			}
			if err := worker.ProcessOnce(t.Context()); err != nil {
				t.Fatalf("process fenced job: %v", err)
			}
			if len(store.dead) != 1 || store.dead[0] != "app-fenced-job" ||
				len(store.deadReasons) != 1 || store.deadReasons[0] != test.wantReason {
				t.Fatalf("dead jobs=%#v reasons=%#v", store.dead, store.deadReasons)
			}
			if len(store.completed) != 0 || len(store.retried) != 0 {
				t.Fatal("app-fenced job was forwarded or retried")
			}
		})
	}
}

func TestWorkerNeverResolvesUnknownAdditionalStaticMessengerJobFromRegistry(t *testing.T) {
	config := newStaticMessengerTestConfig(t)
	store := &fakeQueueStore{jobs: []InboundJob{{
		SchemaVersion:     staticMessengerInboundJobSchemaVersion,
		ID:                "removed-static-page",
		AccountKey:        "removed-page",
		Channel:           models.ChannelMessenger,
		ExternalAccountID: "page-1",
		WebhookApp:        staticMessengerWebhookApp(testStaticMessengerAppID),
		Body:              []byte(`{"external_account_id":"page-1","events":[]}`),
	}}}
	registry := &countingRegistryResolver{}
	worker, err := NewWorker(config, store)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	worker.registry = registry
	if err := worker.ProcessOnce(t.Context()); err != nil {
		t.Fatalf("process removed static account: %v", err)
	}
	if registry.calls.Load() != 0 {
		t.Fatalf("registry calls=%d, want zero", registry.calls.Load())
	}
	if len(store.dead) != 1 || store.dead[0] != "removed-static-page" ||
		len(store.deadReasons) != 1 || store.deadReasons[0] != "unknown_account" {
		t.Fatalf("dead jobs=%#v reasons=%#v", store.dead, store.deadReasons)
	}
	if len(store.completed) != 0 || len(store.retried) != 0 || len(store.parked) != 0 {
		t.Fatal("unknown additional-static job was completed, retried, or parked")
	}
}

func TestAdditionalStaticMessengerDurableIdentitiesAreCutoverIsolated(t *testing.T) {
	raw := testMetaTextWebhookBody(t, 1, "cutover-safe")
	legacyConfig := newTestConfig(t)
	additionalConfig := newStaticMessengerTestConfig(t)
	additionalApp := staticMessengerWebhookApp(testStaticMessengerAppID)

	legacyJobs, err := NormalizeInbound(legacyConfig, WebhookAppMessenger, raw)
	if err != nil {
		t.Fatalf("normalize legacy delivery: %v", err)
	}
	additionalJobs, err := NormalizeInbound(additionalConfig, additionalApp, raw)
	if err != nil {
		t.Fatalf("normalize additional-app delivery: %v", err)
	}
	if len(legacyJobs) != 1 || len(additionalJobs) != 1 {
		t.Fatalf(
			"legacy jobs=%d additional jobs=%d, want one each",
			len(legacyJobs),
			len(additionalJobs),
		)
	}
	if !bytes.Equal(legacyJobs[0].Body, additionalJobs[0].Body) {
		t.Fatal("cutover test did not produce the same canonical body")
	}
	if legacyJobs[0].ID == additionalJobs[0].ID {
		t.Fatal("legacy and additional-app jobs share a durable identity")
	}
	legacyAcceptanceID := inboundAcceptanceID(WebhookAppMessenger, raw)
	additionalAcceptanceID := inboundAcceptanceID(additionalApp, raw)
	if legacyAcceptanceID != digestHex(raw) {
		t.Fatal("legacy acceptance identity changed")
	}
	if legacyAcceptanceID == additionalAcceptanceID {
		t.Fatal("legacy and additional-app deliveries share an acceptance identity")
	}
}

func TestWorkerProcessesNormalizedAdditionalStaticMessengerV3Job(t *testing.T) {
	config := newStaticMessengerTestConfig(t)
	account, ok := config.account(models.ChannelMessenger, "page-1")
	if !ok {
		t.Fatal("additional-app account is missing")
	}
	raw := testMetaTextWebhookBody(t, 1, "forward-v3")
	jobs, err := NormalizeInbound(
		config,
		staticMessengerWebhookApp(testStaticMessengerAppID),
		raw,
	)
	if err != nil {
		t.Fatalf("normalize additional-app delivery: %v", err)
	}
	if len(jobs) != 1 || jobs[0].SchemaVersion != staticMessengerInboundJobSchemaVersion ||
		jobs[0].WebhookApp != staticMessengerWebhookApp(testStaticMessengerAppID) {
		t.Fatalf("normalized jobs=%#v, want one app-fenced v3 job", jobs)
	}

	var received atomic.Bool
	rereply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read ReReply body: %v", readErr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !bytes.Equal(body, jobs[0].Body) ||
			!verifySignedBody(
				account.reReplyInboundSecret,
				request.Header.Get(ReReplySignatureHeader),
				body,
			) {
			t.Error("worker forwarded a changed or incorrectly signed body")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rereply.Close()
	account.ReReplyWebhookURL = rereply.URL

	store := &fakeQueueStore{jobs: jobs}
	worker, err := NewWorker(config, store)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := worker.ProcessOnce(t.Context()); err != nil {
		t.Fatalf("process v3 job: %v", err)
	}
	if !received.Load() || len(store.completed) != 1 || store.completed[0] != jobs[0].ID ||
		len(store.dead) != 0 || len(store.retried) != 0 || len(store.parked) != 0 {
		t.Fatalf(
			"received=%t completed=%#v dead=%#v retried=%#v parked=%#v",
			received.Load(), store.completed, store.dead, store.retried, store.parked,
		)
	}
}
