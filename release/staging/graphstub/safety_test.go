package graphstub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"STUB_ENVIRONMENT":     "staging",
		"STUB_CONTROL_KEY":     testControlKey,
		"STUB_ACCESS_TOKENS":   testToken + ", " + testToken2,
		"STUB_APP_ID":          testAppID,
		"STUB_APP_SECRET":      testAppSecret,
		"STUB_CALLBACK_ORIGIN": "http://omnitech-web/",
	}
}

func configFrom(env map[string]string) (Config, error) {
	return ConfigFromEnv(func(name string) string { return env[name] })
}

func TestConfigFromEnv(t *testing.T) {
	env := validEnv()
	env["STUB_ACCOUNTS"] = `[{"business_account_id":"` + testWABA + `","phone_number_id":"` + testPhone + `","display_phone_number":"+1 555-0101"}]`
	env["STUB_STATUS_SEQUENCE"] = "sent, delivered, read"
	env["STUB_STATUS_INTERVAL"] = "1s"
	config, err := configFrom(env)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if config.ListenAddr != DefaultListenAddr || config.CallbackOrigin != "http://omnitech-web" ||
		len(config.AccessTokens) != 2 || config.Accounts[0].VerifiedName != "Graph Stub Business" ||
		strings.Join(config.StatusSequence, ",") != "sent,delivered,read" || config.StatusInterval != time.Second {
		t.Fatalf("config %+v", config)
	}
	env = validEnv()
	env["STUB_STATUS_SEQUENCE"] = "none"
	if config, err := configFrom(env); err != nil || len(config.StatusSequence) != 0 {
		t.Fatalf("status sequence none: %v %v", config.StatusSequence, err)
	}
	if config, err := configFrom(validEnv()); err != nil || strings.Join(config.StatusSequence, ",") != "sent,delivered" {
		t.Fatalf("default status sequence: %v %v", config.StatusSequence, err)
	}
}

// TestConfigRefusals covers every startup refusal. No error may echo a
// secret value back.
func TestConfigRefusals(t *testing.T) {
	cases := map[string]func(env map[string]string){
		"environment unset":          func(env map[string]string) { delete(env, "STUB_ENVIRONMENT") },
		"environment production":     func(env map[string]string) { env["STUB_ENVIRONMENT"] = "production" },
		"environment test":           func(env map[string]string) { env["STUB_ENVIRONMENT"] = "test" },
		"environment wrong case":     func(env map[string]string) { env["STUB_ENVIRONMENT"] = "Staging" },
		"control key unset":          func(env map[string]string) { delete(env, "STUB_CONTROL_KEY") },
		"control key short":          func(env map[string]string) { env["STUB_CONTROL_KEY"] = "short-control-key" },
		"control key with spaces":    func(env map[string]string) { env["STUB_CONTROL_KEY"] = "a control key with spaces in it, long" },
		"no access tokens":           func(env map[string]string) { env["STUB_ACCESS_TOKENS"] = " , " },
		"short access token":         func(env map[string]string) { env["STUB_ACCESS_TOKENS"] = "short" },
		"app id not numeric":         func(env map[string]string) { env["STUB_APP_ID"] = "app" },
		"app secret short":           func(env map[string]string) { env["STUB_APP_SECRET"] = "short" },
		"app secret is control key":  func(env map[string]string) { env["STUB_APP_SECRET"] = testControlKey },
		"token is app secret":        func(env map[string]string) { env["STUB_ACCESS_TOKENS"] = testAppSecret },
		"token is control key":       func(env map[string]string) { env["STUB_ACCESS_TOKENS"] = testControlKey },
		"callback unset":             func(env map[string]string) { delete(env, "STUB_CALLBACK_ORIGIN") },
		"callback rereply.app":       func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://rereply.app" },
		"callback rereply subdomain": func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://staging.rereply.app" },
		"callback facebook":          func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://graph.facebook.com" },
		"callback fbsbx":             func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://lookaside.fbsbx.com:443" },
		"callback instagram":         func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://INSTAGRAM.COM." },
		"callback googleapis":        func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "https://gmail.googleapis.com" },
		"callback with a path":       func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "http://omnitech-web:8080/api/webhook" },
		"callback with a query":      func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "http://omnitech-web:8080?x=1" },
		"callback with credentials":  func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "http://user:pass@omnitech-web:8080" },
		"callback with a fragment":   func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "http://omnitech-web:8080#x" },
		"callback not http":          func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "ftp://omnitech-web" },
		"callback port zero":         func(env map[string]string) { env["STUB_CALLBACK_ORIGIN"] = "http://omnitech-web:0" },
		"listen address":             func(env map[string]string) { env["STUB_LISTEN_ADDR"] = "8090" },
		"accounts not JSON":          func(env map[string]string) { env["STUB_ACCOUNTS"] = "[{" },
		"accounts unknown field":     func(env map[string]string) { env["STUB_ACCOUNTS"] = `[{"phone":"1"}]` },
		"accounts duplicate phone": func(env map[string]string) {
			env["STUB_ACCOUNTS"] = `[{"business_account_id":"2","phone_number_id":"3","display_phone_number":"+1 555-0101"},` +
				`{"business_account_id":"4","phone_number_id":"3","display_phone_number":"+1 555-0102"}]`
		},
		"status sequence":       func(env map[string]string) { env["STUB_STATUS_SEQUENCE"] = "sent,seen" },
		"status interval":       func(env map[string]string) { env["STUB_STATUS_INTERVAL"] = "soon" },
		"status interval large": func(env map[string]string) { env["STUB_STATUS_INTERVAL"] = "2m" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			mutate(env)
			_, err := configFrom(env)
			if err == nil {
				t.Fatal("accepted")
			}
			for _, secret := range []string{testControlKey, testAppSecret, testToken, "short-control-key", "pass@"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q echoes a secret", err)
				}
			}
		})
	}
	// Lookalikes of refused domains are not refused.
	env := validEnv()
	env["STUB_CALLBACK_ORIGIN"] = "https://notrereply.app"
	if _, err := configFrom(env); err != nil {
		t.Fatalf("a lookalike host was refused: %v", err)
	}
	// New validates too, so a hand-built Config cannot skip the refusals.
	config := testConfig("https://www.facebook.com")
	if _, err := New(config, nil); err == nil {
		t.Fatal("New accepted a production callback origin")
	}
	config = testConfig("http://127.0.0.1:9")
	config.ControlKey = ""
	if _, err := New(config, nil); err == nil {
		t.Fatal("New accepted a missing control key")
	}
}

func TestRefusedHost(t *testing.T) {
	for host, want := range map[string]bool{
		"rereply.app": true, "app.rereply.app:443": true, "graph.facebook.com": true, "FACEBOOK.COM.": true,
		"scontent.fbsbx.com": true, "instagram.com": true, "oauth2.googleapis.com": true,
		"notrereply.app": false, "facebook.com.example.test": false, "graph-stub:8090": false, "127.0.0.1:8090": false, "": false,
	} {
		if got := RefusedHost(host); got != want {
			t.Errorf("RefusedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestHostRefusals: a request addressed to a production host is refused
// before authentication, on the Graph and the control API alike.
func TestHostRefusals(t *testing.T) {
	h := newHarness(t, nil)
	for _, host := range []string{"graph.facebook.com", "api.rereply.app:443", "lookaside.fbsbx.com"} {
		for _, path := range []string{v + "/" + testPhone + "/messages", "/_control/journal", "/oauth/access_token"} {
			request, _ := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(`{}`))
			request.Host = host
			request.Header.Set("Authorization", "Bearer "+testToken)
			if status, _ := h.do(request); status != http.StatusMisdirectedRequest {
				t.Fatalf("%s %s answered %d", host, path, status)
			}
		}
	}
	if entries := h.journal(); len(entries) != 0 {
		t.Fatalf("refused hosts reached the journal: %+v", entries)
	}
	refused := h.rejected()
	if len(refused) != 9 {
		t.Fatalf("%d rejected entries, want 9", len(refused))
	}
	for _, entry := range refused {
		if entry.Kind != KindRefused || entry.Route != "host" {
			t.Fatalf("rejected entry %+v", entry)
		}
	}
	if status, _ := h.graph(http.MethodGet, v+"/"+testPhone, testToken, nil); status != http.StatusOK {
		t.Fatalf("the stub's own host was refused: %d", status)
	}
}

func TestServiceNameCallbackUsesDefaultHTTPPortAndKeepsOriginFence(t *testing.T) {
	client, err := newWebhookClient("http://omnitech-web")
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if port := effectivePort(client.origin); port != "80" {
		t.Fatalf("service-name route port = %q", port)
	}
	if target := client.target("https://other.example.test:8443/hook?workspace=1"); target != "http://omnitech-web/hook?workspace=1" {
		t.Fatalf("callback escaped service-name origin: %q", target)
	}
	for _, address := range []string{"omnitech-web:8080", "other-service:80", "127.0.0.1:80"} {
		if _, err := client.transport.DialContext(context.Background(), "tcp", address); !errors.Is(err, errDialRefused) {
			t.Fatalf("off-origin dial %q: %v", address, err)
		}
	}
}

// TestWebhooksDialOnlyTheCallbackOrigin: the delivery client refuses any
// other address even if handed a URL for one, and never follows a redirect.
func TestWebhooksDialOnlyTheCallbackOrigin(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/webhook", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	client, err := newWebhookClient(redirecting.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if _, err := client.post(context.Background(), other.URL+"/api/webhook", testAppSecret, map[string]any{}); !errors.Is(err, errDialRefused) {
		t.Fatalf("posting to another origin: %v", err)
	}
	status, err := client.post(context.Background(), client.target(""), testAppSecret, map[string]any{})
	if err != nil || status != http.StatusTemporaryRedirect {
		t.Fatalf("redirect: %d %v", status, err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the stub reached a server other than the callback origin")
	}
	if got := client.target("https://other.example.test:8443/hook/path?workspace=1#frag"); got != redirecting.URL+"/hook/path?workspace=1" {
		t.Fatalf("override target %q", got)
	}

	h := newHarness(t, func(config *Config) { config.CallbackOrigin = redirecting.URL })
	response := h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{"phone_number_id": testPhone, "from": testCustomer, "text": "x"})
	if response["callback_status"] != float64(http.StatusTemporaryRedirect) || elsewhere.Load() != 0 {
		t.Fatalf("inbound through a redirect: %v, elsewhere %d", response, elsewhere.Load())
	}

	unreachable := newHarness(t, func(config *Config) { config.CallbackOrigin = "http://127.0.0.1:9" })
	if status, body := unreachable.control(http.MethodPost, "/_control/inbound", map[string]any{"phone_number_id": testPhone, "from": testCustomer, "text": "x"}); status != http.StatusBadGateway {
		t.Fatalf("unreachable product: %d %v", status, body)
	}
}

// TestJournalAndLogsCarryNoBodiesOrSecrets drives every kind of traffic with
// marked content and checks neither the logs nor the journal kept it.
func TestJournalAndLogsCarryNoBodiesOrSecrets(t *testing.T) {
	h := newHarness(t, func(config *Config) { config.StatusSequence = []string{"sent"} })
	const marker = "body-marker-7f3a"
	h.sendText(testToken, testPhone, "outbound "+marker)
	h.product.next(t)
	h.uploadMedia(testPhone, "text/plain", []byte("media "+marker))
	h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{"phone_number_id": testPhone, "from": testCustomer, "text": "inbound " + marker})
	h.product.next(t)
	h.graph(http.MethodPost, v+"/"+testWABA+"/message_templates", testToken, map[string]any{
		"name": "marked", "language": "en", "category": "UTILITY", "components": []any{map[string]any{"type": "BODY", "text": marker}},
	})
	h.graph(http.MethodGet, "/debug_token?input_token="+testToken, testAppID+"|"+testAppSecret, nil)
	h.graph(http.MethodGet, v+"/"+testPhone, "wrong-token-"+marker, nil)

	request, _ := http.NewRequest("PROPFIND-"+strings.ToUpper(marker), h.url+v+"/"+testPhone, nil)
	h.do(request)

	journal, _ := json.Marshal(append(h.journal(), h.rejected()...))
	logs := h.logs.String()
	if !strings.Contains(logs, `"method":"OTHER"`) {
		t.Fatalf("an unknown method was not logged as OTHER: %s", logs)
	}
	if !strings.Contains(logs, `"route":"messages"`) || !strings.Contains(logs, `"msg":"webhook"`) {
		t.Fatalf("logs are missing request lines: %s", logs)
	}
	for _, secret := range []string{marker, strings.ToUpper(marker), testToken, testToken2, testAppSecret, testControlKey, testCustomer + "\""} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
	for _, secret := range []string{marker, strings.ToUpper(marker), testToken, testToken2, testAppSecret, testControlKey} {
		if strings.Contains(string(journal), secret) {
			t.Errorf("journal contains %q", secret)
		}
	}
	textHashes := 0
	for _, entry := range h.journal() {
		if entry.TextSHA256 != "" {
			textHashes++
		}
	}
	if textHashes != 2 {
		t.Fatalf("%d entries carry a text hash, want the outbound and the inbound", textHashes)
	}
}

// TestUnauthenticatedRequestsStayOutOfTheJournal: on staging /_stub is
// public, so anyone can send requests that fail authentication. They go to
// the small rejected ring, and the journal a test run reads does not move.
func TestUnauthenticatedRequestsStayOutOfTheJournal(t *testing.T) {
	h := newHarness(t, nil)
	h.graph(http.MethodGet, v+"/"+testPhone, testToken, nil)
	_, before := h.stub.journal.since(0, 1)
	flood := []func(){
		func() {
			// A current timestamp and a fresh nonce, but not the key.
			request, _ := http.NewRequest(http.MethodGet, h.url+"/_control/journal", nil)
			SignControlRequest(request, "not-the-control-key-but-long-enough-0", nil, time.Now())
			h.do(request)
		},
		func() { h.graph(http.MethodGet, v+"/"+testPhone, "wrong-token-value-0000", nil) },
		func() { h.graph(http.MethodGet, v+"/"+testPhone, "", nil) },
		func() { h.graph(http.MethodGet, "/", "", nil) },
		func() { h.graph(http.MethodPost, "/oauth/access_token?client_id=1&client_secret=wrong", "", nil) },
		func() { h.graph(http.MethodGet, "/debug_token?input_token=x", "wrong-app-token", nil) },
		func() {
			request, _ := http.NewRequest(http.MethodGet, h.url+"/_control/journal", nil)
			request.Host = "graph.facebook.com"
			h.do(request)
		},
	}
	rounds := RejectedCapacity/len(flood) + 2
	for round := 0; round < rounds; round++ {
		for _, request := range flood {
			request()
		}
	}
	if _, after := h.stub.journal.since(0, 1); after != before {
		t.Fatalf("unauthenticated requests moved the journal from %d to %d", before, after)
	}
	if rejected := h.rejected(); len(rejected) != RejectedCapacity {
		t.Fatalf("the rejected ring holds %d entries, want %d", len(rejected), RejectedCapacity)
	}
	page := h.mustControl(http.MethodGet, "/_control/rejected?limit=5", nil)
	if entries, _ := page["entries"].([]any); len(entries) != 5 || page["latest"] != float64(rounds*len(flood)) {
		t.Fatalf("rejected page %v", page)
	}
	if entries := h.journal(); len(entries) != 2 || entries[0].Route != routePhone || entries[1].Route != "rejected" {
		t.Fatalf("journal %+v", entries)
	}
	h.mustControl(http.MethodPost, "/_control/reset", nil)
	if rejected := h.rejected(); len(rejected) != 0 {
		t.Fatalf("reset kept %d rejected entries", len(rejected))
	}
}

func TestJournalIsATenThousandEntryRing(t *testing.T) {
	journal := newJournal(JournalCapacity)
	for index := 0; index < JournalCapacity+25; index++ {
		journal.add(Entry{Kind: KindGraph, Route: "phone", To: strings.Repeat("x", 500)})
	}
	entries, latest := journal.since(0, JournalCapacity+100)
	if len(entries) != JournalCapacity || latest != JournalCapacity+25 || entries[0].Seq != 26 || entries[len(entries)-1].Seq != latest {
		t.Fatalf("%d entries, first %d, latest %d", len(entries), entries[0].Seq, latest)
	}
	if len(entries[0].To) != maxJournalField {
		t.Fatalf("a journal field kept %d bytes", len(entries[0].To))
	}
	if page, _ := journal.since(latest-3, 2); len(page) != 2 || page[0].Seq != latest-2 {
		t.Fatalf("page %+v", page)
	}
	journal.reset()
	if entries, latest := journal.since(0, 10); len(entries) != 0 || latest != JournalCapacity+25 {
		t.Fatalf("after reset: %d entries, latest %d", len(entries), latest)
	}
	if seq := journal.add(Entry{}); seq != JournalCapacity+26 {
		t.Fatalf("sequence restarted at %d", seq)
	}
}

func TestHealthEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		if status, body := h.graph(http.MethodGet, path, "", nil); status != http.StatusOK || body["status"] != "ok" {
			t.Fatalf("%s: %d %v", path, status, body)
		}
	}
	if entries := h.journal(); len(entries) != 0 {
		t.Fatalf("health probes were journaled: %+v", entries)
	}
}

func TestCloseStopsScheduledStatuses(t *testing.T) {
	h := newHarness(t, func(config *Config) {
		config.StatusSequence = []string{"sent", "delivered"}
		config.StatusInterval = time.Minute
	})
	h.sendText(testToken, testPhone, "never delivered")
	done := make(chan struct{})
	go func() {
		h.stub.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a scheduled status")
	}
	h.product.none(t, 20*time.Millisecond)
	if h.stub.scheduleStatuses(Account{PhoneNumberID: testPhone}, sentMessage{ID: "wamid.after-close"}, 0) {
		t.Fatal("a status sequence was scheduled after Close")
	}
}
