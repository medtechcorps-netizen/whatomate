package graphstub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestControlRequiresAValidMAC covers every way a control request can fail
// authentication; each one is a 401 and none of them reaches a handler.
func TestControlRequiresAValidMAC(t *testing.T) {
	h := newHarness(t, nil)
	body := []byte(`{"route":"messages","status":503}`)
	// signing is what the client signs; the request itself is always a POST
	// of body to /_control/faults.
	type signing struct {
		method, path, timestamp, nonce, key string
		body                                []byte
	}
	signed := func(mutate func(*signing)) *http.Request {
		request, _ := http.NewRequest(http.MethodPost, h.url+"/_control/faults", bytes.NewReader(body))
		values := signing{
			method: http.MethodPost, path: "/_control/faults", timestamp: strconv.FormatInt(time.Now().Unix(), 10),
			nonce: randomToken(18), key: testControlKey, body: body,
		}
		mutate(&values)
		request.Header.Set(HeaderTimestamp, values.timestamp)
		request.Header.Set(HeaderNonce, values.nonce)
		request.Header.Set(HeaderMAC, ControlMAC(values.key, values.method, values.path, values.timestamp, values.nonce, values.body))
		return request
	}
	noop := func(*signing) {}
	cases := map[string]*http.Request{
		"wrong key": signed(func(s *signing) { s.key = "another-control-key-that-is-long-enough" }),
		"stale timestamp": signed(func(s *signing) {
			s.timestamp = strconv.FormatInt(time.Now().Add(-ControlSkew-5*time.Second).Unix(), 10)
		}),
		"future timestamp": signed(func(s *signing) {
			s.timestamp = strconv.FormatInt(time.Now().Add(ControlSkew+5*time.Second).Unix(), 10)
		}),
		"body changed after signing": signed(func(s *signing) { s.body = []byte(`{"route":"messages","status":500}`) }),
		"different path signed":      signed(func(s *signing) { s.path = "/_control/reset" }),
		"different method signed":    signed(func(s *signing) { s.method = http.MethodDelete }),
		"short nonce":                signed(func(s *signing) { s.nonce = "short" }),
		"non-numeric timestamp":      signed(func(s *signing) { s.timestamp = "1e9" }),
	}
	for _, header := range []string{HeaderTimestamp, HeaderNonce, HeaderMAC} {
		request := signed(noop)
		request.Header.Del(header)
		cases["missing "+header] = request
	}
	uppercase := signed(noop)
	uppercase.Header.Set(HeaderMAC, strings.ToUpper(uppercase.Header.Get(HeaderMAC)))
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			status, decoded := h.do(request)
			if status != http.StatusUnauthorized || decoded["error"] != "unauthorized" {
				t.Fatalf("status %d %v, want 401", status, decoded)
			}
		})
	}
	h.stub.mu.Lock()
	queued := len(h.stub.faults)
	h.stub.mu.Unlock()
	if queued != 0 {
		t.Fatalf("an unauthenticated request queued %d faults", queued)
	}
	if status, decoded := h.do(uppercase); status != http.StatusOK {
		t.Fatalf("a valid request (upper-case hex MAC) was refused: %d %v", status, decoded)
	}
	reasons := map[string]bool{}
	for _, entry := range h.journal() {
		if entry.Kind == KindControl {
			reasons[entry.Error] = true
		}
	}
	for _, reason := range []string{"missing_header", "malformed_header", "stale_timestamp", "bad_mac"} {
		if !reasons[reason] {
			t.Errorf("no control journal entry with error %q", reason)
		}
	}
}

func TestControlRefusesAReplayedNonce(t *testing.T) {
	h := newHarness(t, nil)
	request, _ := http.NewRequest(http.MethodGet, h.url+"/_control/journal?limit=5", nil)
	SignControlRequest(request, testControlKey, nil, time.Now())
	if status, body := h.do(request); status != http.StatusOK {
		t.Fatalf("first use: %d %v", status, body)
	}
	replay, _ := http.NewRequest(http.MethodGet, h.url+"/_control/journal?limit=5", nil)
	replay.Header = request.Header.Clone()
	if status, body := h.do(replay); status != http.StatusUnauthorized {
		t.Fatalf("replay: %d %v", status, body)
	}
	if entries := h.journal(); entries[len(entries)-1].Error != "replayed_nonce" {
		t.Fatalf("last journal entry %+v", entries[len(entries)-1])
	}
}

// readTracker records whether the server ever read the request body.
type readTracker struct {
	reader io.Reader
	read   bool
}

func (r *readTracker) Read(data []byte) (int, error) {
	r.read = true
	return r.reader.Read(data)
}

func TestControlChecksTheClockBeforeReadingTheBody(t *testing.T) {
	h := newHarness(t, nil)
	for name, mutate := range map[string]func(*http.Request){
		"stale": func(r *http.Request) {
			r.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
		},
		"unsigned":  func(r *http.Request) { r.Header.Del(HeaderMAC) },
		"malformed": func(r *http.Request) { r.Header.Set(HeaderNonce, "not a nonce") },
	} {
		t.Run(name, func(t *testing.T) {
			body := &readTracker{reader: strings.NewReader(`{"route":"any","status":500}`)}
			request := httptest.NewRequest(http.MethodPost, "/_control/faults", body)
			SignControlRequest(request, testControlKey, []byte(`{"route":"any","status":500}`), time.Now())
			mutate(request)
			recorder := httptest.NewRecorder()
			h.stub.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized || body.read {
				t.Fatalf("status %d, body read %v", recorder.Code, body.read)
			}
		})
	}
}

func TestControlBodyLimit(t *testing.T) {
	h := newHarness(t, nil)
	body := bytes.Repeat([]byte("a"), maxControlBody+1)
	request, _ := http.NewRequest(http.MethodPost, h.url+"/_control/media", bytes.NewReader(body))
	SignControlRequest(request, testControlKey, body, time.Now())
	if status, _ := h.do(request); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", status)
	}
}

func TestNonceCacheFailsClosedWhenFull(t *testing.T) {
	cache := newNonceCache()
	now := time.Now()
	for index := 0; index < maxNonces; index++ {
		if accepted, _ := cache.use("nonce-"+strconv.Itoa(index)+"-padding", now); !accepted {
			t.Fatalf("nonce %d refused", index)
		}
	}
	if accepted, full := cache.use("one-more-nonce-value", now.Add(ControlSkew)); accepted || !full {
		t.Fatalf("a full cache of live nonces accepted another (accepted %v, full %v)", accepted, full)
	}
	if accepted, full := cache.use("one-more-nonce-value", now.Add(2*ControlSkew+time.Second)); !accepted || full {
		t.Fatalf("expired nonces were not purged (accepted %v, full %v)", accepted, full)
	}
	if accepted, _ := cache.use("one-more-nonce-value", now.Add(2*ControlSkew+2*time.Second)); accepted {
		t.Fatal("a nonce was accepted twice")
	}
}

func TestControlRoutes(t *testing.T) {
	h := newHarness(t, nil)
	if status, body := h.control(http.MethodGet, "/_control/nope", nil); status != http.StatusNotFound {
		t.Fatalf("unknown route: %d %v", status, body)
	}
	if status, body := h.control(http.MethodGet, "/_control/reset", nil); status != http.StatusNotFound {
		t.Fatalf("wrong method: %d %v", status, body)
	}
	if status, body := h.control(http.MethodPost, "/_control/accounts", map[string]any{
		"business_account_id": testWABA, "phone_number_id": testPhone, "display_phone_number": "+1 555-0101", "unexpected": true,
	}); status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %v", status, body)
	}
}

func TestControlAccounts(t *testing.T) {
	h := newHarness(t, nil)
	const phone, waba = "300000000000099", "200000000000099"
	if status, _ := h.graph(http.MethodGet, v+"/"+phone, testToken, nil); status != http.StatusBadRequest {
		t.Fatalf("unregistered phone answered %d", status)
	}
	account := map[string]any{"business_account_id": waba, "phone_number_id": phone, "display_phone_number": "+1 555-0199", "verified_name": "Second Clinic"}
	h.mustControl(http.MethodPost, "/_control/accounts", account)
	h.mustControl(http.MethodPost, "/_control/accounts", account) // idempotent
	if status, body := h.graph(http.MethodGet, v+"/"+phone+"?fields=verified_name", testToken, nil); status != http.StatusOK || body["verified_name"] != "Second Clinic" {
		t.Fatalf("registered phone: %d %v", status, body)
	}
	if status, body := h.graph(http.MethodGet, v+"/"+waba+"/phone_numbers", testToken, nil); status != http.StatusOK || dig(body, "data", 0, "id") != phone {
		t.Fatalf("registered WABA: %d %v", status, body)
	}
	for name, bad := range map[string]map[string]any{
		"phone moved to another WABA": {"business_account_id": testWABA2, "phone_number_id": phone, "display_phone_number": "+1 555-0199"},
		"non-numeric phone":           {"business_account_id": waba, "phone_number_id": "phone-1", "display_phone_number": "+1 555-0199"},
		"WABA that is a phone":        {"business_account_id": testPhone, "phone_number_id": "300000000000098", "display_phone_number": "+1 555-0198"},
		"bad display number":          {"business_account_id": waba, "phone_number_id": "300000000000097", "display_phone_number": "call me"},
	} {
		if status, body := h.control(http.MethodPost, "/_control/accounts", bad); status != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	listed := h.mustControl(http.MethodGet, "/_control/accounts", nil)
	if accounts, _ := listed["accounts"].([]any); len(accounts) != 4 || dig(listed, "accounts", 3, "phone_number_id") != phone {
		t.Fatalf("accounts %v", listed)
	}
}

// TestControlInboundMatchesTheCanaryEnvelope checks the inbound payload has
// the shape frontend/canary-driver/runner.mjs signs today, is signed with the
// app secret, and that the product's answer is reported back.
func TestControlInboundMatchesTheCanaryEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	response := h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{
		"phone_number_id": testPhone, "from": testCustomer, "profile_name": "Synthetic Patient",
		"text": "hello from the stub", "message_id": "wamid.synthetic-inbound-1", "timestamp": 1790000000,
	})
	if response["callback_status"] != float64(http.StatusOK) || response["message_id"] != "wamid.synthetic-inbound-1" {
		t.Fatalf("inbound response %v", response)
	}
	webhook := h.product.next(t)
	if webhook.Path != WebhookPath || webhook.Signature != SignWebhook(testAppSecret, webhook.Body) {
		t.Fatalf("webhook to %q with signature %q", webhook.Path, webhook.Signature)
	}
	want := map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": testWABA,
			"changes": []any{map[string]any{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata":          map[string]any{"display_phone_number": "+1 555-0101", "phone_number_id": testPhone},
					"contacts":          []any{map[string]any{"profile": map[string]any{"name": "Synthetic Patient"}, "wa_id": testCustomer}},
					"messages": []any{map[string]any{
						"from": testCustomer, "id": "wamid.synthetic-inbound-1", "timestamp": "1790000000",
						"text": map[string]any{"body": "hello from the stub"}, "type": "text",
					}},
				},
			}},
		}},
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(webhook.decode(t))
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("inbound envelope\n got %s\nwant %s", gotJSON, wantJSON)
	}

	h.product.status.Store(http.StatusForbidden)
	response = h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{
		"phone_number_id": testPhone, "from": testCustomer, "text": "reply", "context_message_id": "wamid.out-1",
	})
	if response["callback_status"] != float64(http.StatusForbidden) {
		t.Fatalf("the product's 403 was not reported: %v", response)
	}
	if context := dig(h.product.next(t).decode(t), "entry", 0, "changes", 0, "value", "messages", 0, "context"); dig(context, "id") != "wamid.out-1" || dig(context, "from") != "15550101" {
		t.Fatalf("reply context %v", context)
	}

	for name, bad := range map[string]map[string]any{
		"unknown phone":     {"phone_number_id": "399999999999999", "from": testCustomer, "text": "x"},
		"non-digit sender":  {"phone_number_id": testPhone, "from": "bsuid:abc", "text": "x"},
		"empty text":        {"phone_number_id": testPhone, "from": testCustomer},
		"unknown type":      {"phone_number_id": testPhone, "from": testCustomer, "type": "sticker"},
		"bad message id":    {"phone_number_id": testPhone, "from": testCustomer, "text": "x", "message_id": "has space"},
		"image never saved": {"phone_number_id": testPhone, "from": testCustomer, "type": "image", "media_id": "123456789012345"},
	} {
		if status, body := h.control(http.MethodPost, "/_control/inbound", bad); status != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	h.product.none(t, 20*time.Millisecond)
}

// TestControlInboundMediaRoundTrip stores media for an inbound image, then
// fetches it the way the product does: metadata, then the same-origin URL.
func TestControlInboundMediaRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	payload := []byte("synthetic jpeg bytes \x00\xff")
	stored := h.mustControl(http.MethodPost, "/_control/media", map[string]any{
		"phone_number_id": testPhone, "mime_type": "image/jpeg", "data_base64": base64.StdEncoding.EncodeToString(payload),
	})
	mediaID, _ := stored["id"].(string)
	h.mustControl(http.MethodPost, "/_control/inbound", map[string]any{
		"phone_number_id": testPhone, "from": testCustomer, "type": "image", "media_id": mediaID, "caption": "x-ray",
	})
	image := dig(h.product.next(t).decode(t), "entry", 0, "changes", 0, "value", "messages", 0, "image")
	if dig(image, "id") != mediaID || dig(image, "mime_type") != "image/jpeg" || dig(image, "sha256") != stored["sha256"] || dig(image, "caption") != "x-ray" {
		t.Fatalf("inbound image %v", image)
	}
	_, metadata := h.graph(http.MethodGet, v+"/"+mediaID, testToken, nil)
	request, _ := http.NewRequest(http.MethodGet, metadata["url"].(string), nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(downloaded, payload) {
		t.Fatalf("downloaded %q", downloaded)
	}
	if status, body := h.control(http.MethodPost, "/_control/media", map[string]any{
		"phone_number_id": testPhone, "mime_type": "image/jpeg", "data_base64": "%%%",
	}); status != http.StatusBadRequest {
		t.Fatalf("bad base64: %d %v", status, body)
	}
}

func TestScheduledStatusWebhooks(t *testing.T) {
	h := newHarness(t, func(config *Config) {
		config.StatusSequence = []string{"sent", "delivered", "read"}
	})
	_, sent := h.sendText(testToken, testPhone, "status me")
	wamid := dig(sent, "messages", 0, "id")
	for _, want := range []string{"sent", "delivered", "read"} {
		webhook := h.product.next(t)
		if webhook.Signature != SignWebhook(testAppSecret, webhook.Body) {
			t.Fatal("status webhook signature differs")
		}
		status := dig(webhook.decode(t), "entry", 0, "changes", 0, "value", "statuses", 0)
		if dig(status, "id") != wamid || dig(status, "status") != want || dig(status, "recipient_id") != testCustomer {
			t.Fatalf("status %v, want %s", status, want)
		}
	}
	h.product.none(t, 30*time.Millisecond)

	// /_control/status delivers one more, for any status, synchronously.
	response := h.mustControl(http.MethodPost, "/_control/status", map[string]any{"message_id": wamid, "status": "failed"})
	if response["callback_status"] != float64(http.StatusOK) {
		t.Fatalf("status response %v", response)
	}
	failed := dig(h.product.next(t).decode(t), "entry", 0, "changes", 0, "value", "statuses", 0)
	if dig(failed, "status") != "failed" || dig(failed, "errors", 0, "code") != float64(131026) {
		t.Fatalf("failed status %v", failed)
	}
	if status, body := h.control(http.MethodPost, "/_control/status", map[string]any{"message_id": "wamid.unknown", "status": "read"}); status != http.StatusBadRequest {
		t.Fatalf("unknown message: %d %v", status, body)
	}
}

func TestScheduledStatusesFollowTheWebhookOverride(t *testing.T) {
	h := newHarness(t, func(config *Config) { config.StatusSequence = []string{"sent"} })
	if status, body := h.graph(http.MethodPost, v+"/"+testPhone, testToken, map[string]any{
		"webhook_configuration": map[string]any{
			"override_callback_uri": "https://elsewhere.example.test/api/webhook?workspace=synthetic", "verify_token": "verify-token-value",
		},
	}); status != http.StatusOK {
		t.Fatalf("override: %d %v", status, body)
	}
	h.sendText(testToken, testPhone, "routed")
	// The override's path and query are used; its host never is.
	if webhook := h.product.next(t); webhook.Path != "/api/webhook?workspace=synthetic" {
		t.Fatalf("delivered to %q", webhook.Path)
	}
	h.sendText(testToken, testPhone2, "not overridden")
	if webhook := h.product.next(t); webhook.Path != WebhookPath {
		t.Fatalf("delivered to %q", webhook.Path)
	}
}

func TestControlTemplateReview(t *testing.T) {
	h := newHarness(t, nil)
	for _, language := range []string{"en", "ms"} {
		if status, body := h.graph(http.MethodPost, v+"/"+testWABA+"/message_templates", testToken, map[string]any{
			"name": "visit_reminder", "language": language, "category": "UTILITY",
			"components": []any{map[string]any{"type": "BODY", "text": "Hello"}},
		}); status != http.StatusOK {
			t.Fatalf("create %s: %d %v", language, status, body)
		}
	}
	templateSend := map[string]any{
		"messaging_product": "whatsapp", "to": testCustomer, "type": "template",
		"template": map[string]any{"name": "visit_reminder", "language": map[string]any{"code": "en"}},
	}
	if status, _ := h.graph(http.MethodPost, v+"/"+testPhone+"/messages", testToken, templateSend); status != http.StatusNotFound {
		t.Fatalf("a pending template was sendable: %d", status)
	}
	response := h.mustControl(http.MethodPost, "/_control/templates", map[string]any{
		"business_account_id": testWABA, "name": "visit_reminder", "language": "en", "status": "APPROVED",
	})
	if response["updated"] != float64(1) {
		t.Fatalf("review response %v", response)
	}
	value := dig(h.product.next(t).decode(t), "entry", 0, "changes", 0)
	if dig(value, "field") != "message_template_status_update" || dig(value, "value", "event") != "APPROVED" ||
		dig(value, "value", "message_template_name") != "visit_reminder" || dig(value, "value", "message_template_language") != "en" {
		t.Fatalf("template webhook %v", value)
	}
	if id, ok := dig(value, "value", "message_template_id").(float64); !ok || id < 1e14 {
		t.Fatalf("message_template_id %v is not a numeric Graph ID", dig(value, "value", "message_template_id"))
	}
	h.product.none(t, 20*time.Millisecond)
	if status, body := h.graph(http.MethodPost, v+"/"+testPhone+"/messages", testToken, templateSend); status != http.StatusOK {
		t.Fatalf("an approved template send: %d %v", status, body)
	}
	if status, body := h.graph(http.MethodPost, v+"/"+testPhone3+"/messages", testToken, templateSend); status != http.StatusNotFound {
		t.Fatalf("a template of another WABA was sendable: %d %v", status, body)
	}

	// Without a language every language is reviewed.
	if response := h.mustControl(http.MethodPost, "/_control/templates", map[string]any{
		"business_account_id": testWABA, "name": "visit_reminder", "status": "PAUSED", "reason": "LOW_QUALITY",
	}); response["updated"] != float64(2) {
		t.Fatalf("review response %v", response)
	}
	h.product.next(t)
	h.product.next(t)
	if status, body := h.control(http.MethodPost, "/_control/templates", map[string]any{
		"business_account_id": testWABA, "name": "missing", "status": "APPROVED",
	}); status != http.StatusBadRequest {
		t.Fatalf("unknown template: %d %v", status, body)
	}
}

func TestControlReset(t *testing.T) {
	h := newHarness(t, func(config *Config) {
		config.StatusSequence = []string{"sent"}
		config.StatusInterval = 200 * time.Millisecond
	})
	h.mustControl(http.MethodPost, "/_control/accounts", map[string]any{
		"business_account_id": "200000000000099", "phone_number_id": "300000000000099", "display_phone_number": "+1 555-0199",
	})
	_, uploaded := h.uploadMedia(testPhone, "image/png", []byte("png"))
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeWABA, "status": 500})
	h.sendText(testToken, testPhone, "pending status")

	h.mustControl(http.MethodPost, "/_control/reset", nil)

	if status, _ := h.graph(http.MethodGet, v+"/300000000000099", testToken, nil); status != http.StatusBadRequest {
		t.Fatalf("a runtime account survived reset: %d", status)
	}
	if status, _ := h.graph(http.MethodGet, v+"/"+uploaded["id"].(string), testToken, nil); status != http.StatusBadRequest {
		t.Fatalf("media survived reset: %d", status)
	}
	if status, _ := h.graph(http.MethodGet, v+"/"+testWABA, testToken, nil); status != http.StatusOK {
		t.Fatalf("a fault or a configured account did not survive reset correctly: %d", status)
	}
	// The send's scheduled status belongs to the previous generation.
	h.product.none(t, 400*time.Millisecond)
	for _, entry := range h.journal() {
		if entry.Route == routeMessages || entry.Route == routeMediaUpload {
			t.Fatalf("pre-reset entry kept: %+v", entry)
		}
	}
}

func TestControlJournalPaging(t *testing.T) {
	h := newHarness(t, nil)
	for index := 0; index < 5; index++ {
		h.graph(http.MethodGet, v+"/"+testPhone, testToken, nil)
	}
	first := h.mustControl(http.MethodGet, "/_control/journal?limit=2", nil)
	entries, _ := first["entries"].([]any)
	if len(entries) != 2 || dig(entries, 0, "seq") != float64(1) || dig(entries, 0, "route") != routePhone {
		t.Fatalf("first page %v", first)
	}
	second := h.mustControl(http.MethodGet, "/_control/journal?after=2&limit=1000", nil)
	entries, _ = second["entries"].([]any)
	// Three more Graph reads plus the first journal request itself.
	if len(entries) != 4 || dig(entries, 0, "seq") != float64(3) || dig(entries, 3, "kind") != KindControl {
		t.Fatalf("second page %v", second)
	}
	if status, _ := h.control(http.MethodGet, "/_control/journal?limit=1001", nil); status != http.StatusBadRequest {
		t.Fatalf("limit over 1000 accepted: %d", status)
	}
}

func TestFaultInjection(t *testing.T) {
	h := newHarness(t, nil)
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{
		"route": routeMessages, "status": 503, "code": 131000, "message": "Something went wrong", "transient": true, "times": 2,
	})
	for attempt := 0; attempt < 2; attempt++ {
		status, body := h.sendText(testToken, testPhone, "faulted")
		if status != http.StatusServiceUnavailable || errorCode(t, body) != 131000 || dig(body, "error", "is_transient") != true {
			t.Fatalf("attempt %d: %d %v", attempt, status, body)
		}
	}
	if status, body := h.sendText(testToken, testPhone, "recovered"); status != http.StatusOK {
		t.Fatalf("after the fault: %d %v", status, body)
	}
	faulted := 0
	for _, entry := range h.journal() {
		if entry.Fault {
			faulted++
		}
	}
	if faulted != 2 {
		t.Fatalf("%d journal entries marked as faults, want 2", faulted)
	}

	// A fault on one route leaves the others alone; "any" takes the next request.
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeTemplates, "status": 400})
	if status, _ := h.graph(http.MethodGet, v+"/"+testPhone, testToken, nil); status != http.StatusOK {
		t.Fatalf("a template fault hit a phone read: %d", status)
	}
	if status, body := h.graph(http.MethodGet, v+"/"+testWABA+"/message_templates", testToken, nil); status != http.StatusBadRequest || errorCode(t, body) != 100 {
		t.Fatalf("template fault: %d %v", status, body)
	}
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeAny, "status": 500})
	if status, body := h.graph(http.MethodGet, v+"/"+testWABA, testToken, nil); status != http.StatusInternalServerError || errorCode(t, body) != 1 {
		t.Fatalf("any fault: %d %v", status, body)
	}

	// drop closes the connection: the caller sees a transport error.
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeMessages, "drop": true})
	request, _ := http.NewRequest(http.MethodPost, h.url+v+"/"+testPhone+"/messages", strings.NewReader(`{"messaging_product":"whatsapp"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	if response, err := h.server.Client().Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatalf("a dropped request answered %d", response.StatusCode)
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
		t.Logf("drop surfaced as %v", err)
	}

	// delay_ms alone stalls the request and then serves it.
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routePhone, "delay_ms": 150})
	started := time.Now()
	if status, _ := h.graph(http.MethodGet, v+"/"+testPhone, testToken, nil); status != http.StatusOK || time.Since(started) < 150*time.Millisecond {
		t.Fatalf("delay fault: %d after %s", status, time.Since(started))
	}

	// Faults apply to the token endpoints too.
	h.mustControl(http.MethodPost, "/_control/faults", map[string]any{"route": routeDebugToken, "status": 500, "times": 3})
	if status, _ := h.graph(http.MethodGet, "/debug_token?input_token="+testToken, testAppID+"|"+testAppSecret, nil); status != http.StatusInternalServerError {
		t.Fatalf("debug_token fault: %d", status)
	}
	if cleared := h.mustControl(http.MethodDelete, "/_control/faults", nil); cleared["cleared"] != float64(1) {
		t.Fatalf("clear %v", cleared)
	}
	if status, _ := h.graph(http.MethodGet, "/debug_token?input_token="+testToken, testAppID+"|"+testAppSecret, nil); status != http.StatusOK {
		t.Fatalf("after clearing: %d", status)
	}

	for name, bad := range map[string]map[string]any{
		"unknown route":    {"route": "everything", "status": 500},
		"success status":   {"route": routeMessages, "status": 200},
		"status and drop":  {"route": routeMessages, "status": 500, "drop": true},
		"nothing to do":    {"route": routeMessages},
		"too many times":   {"route": routeMessages, "status": 500, "times": 1001},
		"delay too long":   {"route": routeMessages, "delay_ms": 30001},
		"unprintable text": {"route": routeMessages, "status": 500, "message": "line\nbreak"},
	} {
		if status, body := h.control(http.MethodPost, "/_control/faults", bad); status != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
}
