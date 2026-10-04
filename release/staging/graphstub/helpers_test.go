package graphstub

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic values only. Graph IDs are numeric like Meta's; phone numbers use
// the reserved 555-01xx range.
const (
	testControlKey = "graph-stub-test-control-key-0123456789"
	testToken      = "graph-stub-test-access-token-01"
	testToken2     = "graph-stub-test-access-token-02"
	testAppID      = "100000000000777"
	testAppSecret  = "graph-stub-test-app-secret-01"
	testWABA       = "200000000000001"
	testPhone      = "300000000000001"
	testPhone2     = "300000000000002"
	testWABA2      = "200000000000002"
	testPhone3     = "300000000000003"
	testCustomer   = "15550100123"
	v              = "/v21.0"
)

func testConfig(callbackOrigin string) Config {
	return Config{
		Environment:    EnvironmentLocal,
		AccessTokens:   []string{testToken, testToken2},
		ControlKey:     testControlKey,
		AppID:          testAppID,
		AppSecret:      testAppSecret,
		CallbackOrigin: callbackOrigin,
		Accounts: []Account{
			{BusinessAccountID: testWABA, PhoneNumberID: testPhone, DisplayPhoneNumber: "+1 555-0101", VerifiedName: "Stub Clinic"},
			{BusinessAccountID: testWABA, PhoneNumberID: testPhone2, DisplayPhoneNumber: "+1 555-0102"},
			{BusinessAccountID: testWABA2, PhoneNumberID: testPhone3, DisplayPhoneNumber: "+1 555-0103"},
		},
		StatusSequence: nil,
		StatusInterval: 5 * time.Millisecond,
	}
}

// capturedWebhook is one webhook the fake product received.
type capturedWebhook struct {
	Path      string
	Body      []byte
	Signature string
}

func (w capturedWebhook) decode(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(w.Body, &value); err != nil {
		t.Fatalf("webhook body is not JSON: %v", err)
	}
	return value
}

// fakeProduct stands in for the product's webhook endpoint.
type fakeProduct struct {
	server   *httptest.Server
	received chan capturedWebhook
	status   atomic.Int32
}

func newFakeProduct(t *testing.T) *fakeProduct {
	t.Helper()
	product := &fakeProduct{received: make(chan capturedWebhook, 64)}
	product.status.Store(http.StatusOK)
	product.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		product.received <- capturedWebhook{Path: r.URL.RequestURI(), Body: body, Signature: r.Header.Get(SignatureHeader)}
		w.WriteHeader(int(product.status.Load()))
	}))
	t.Cleanup(product.server.Close)
	return product
}

func (p *fakeProduct) next(t *testing.T) capturedWebhook {
	t.Helper()
	select {
	case webhook := <-p.received:
		return webhook
	case <-time.After(5 * time.Second):
		t.Fatal("the product received no webhook")
		return capturedWebhook{}
	}
}

func (p *fakeProduct) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case webhook := <-p.received:
		t.Fatalf("unexpected webhook to %s", webhook.Path)
	case <-time.After(wait):
	}
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type harness struct {
	t       *testing.T
	stub    *Server
	server  *httptest.Server
	url     string
	product *fakeProduct
	logs    *lockedBuffer
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	product := newFakeProduct(t)
	config := testConfig(product.server.URL)
	if mutate != nil {
		mutate(&config)
	}
	logs := &lockedBuffer{}
	stub, err := New(config, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server := httptest.NewServer(stub)
	t.Cleanup(func() {
		server.Close()
		stub.Close()
	})
	return &harness{t: t, stub: stub, server: server, url: server.URL, product: product, logs: logs}
}

func (h *harness) do(request *http.Request) (int, map[string]any) {
	h.t.Helper()
	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	if len(raw) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			h.t.Fatalf("%s %s: response is not JSON: %v", request.Method, request.URL.Path, err)
		}
	}
	return response.StatusCode, decoded
}

func encodeBody(t *testing.T, body any) []byte {
	t.Helper()
	switch value := body.(type) {
	case nil:
		return nil
	case []byte:
		return value
	case string:
		return []byte(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
}

// graph sends a Graph request with token as its bearer (none when empty).
func (h *harness) graph(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	request, err := http.NewRequest(method, h.url+path, bytes.NewReader(encodeBody(h.t, body)))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return h.do(request)
}

// control sends a correctly signed control request.
func (h *harness) control(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	encoded := encodeBody(h.t, body)
	request, err := http.NewRequest(method, h.url+path, bytes.NewReader(encoded))
	if err != nil {
		h.t.Fatal(err)
	}
	SignControlRequest(request, testControlKey, encoded, time.Now())
	return h.do(request)
}

func (h *harness) mustControl(method, path string, body any) map[string]any {
	h.t.Helper()
	status, decoded := h.control(method, path, body)
	if status != http.StatusOK {
		h.t.Fatalf("%s %s: %d %v", method, path, status, decoded)
	}
	return decoded
}

func (h *harness) sendText(token, phoneID, text string) (int, map[string]any) {
	h.t.Helper()
	return h.graph(http.MethodPost, v+"/"+phoneID+"/messages", token, map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                testCustomer,
		"type":              "text",
		"text":              map[string]any{"body": text},
	})
}

func (h *harness) uploadMedia(phoneID, mimeType string, data []byte) (int, map[string]any) {
	h.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("messaging_product", "whatsapp")
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="sample.bin"`)
	header.Set("Content-Type", mimeType)
	part, _ := writer.CreatePart(header)
	_, _ = part.Write(data)
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, h.url+v+"/"+phoneID+"/media", &body)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return h.do(request)
}

func (h *harness) journal() []Entry {
	h.t.Helper()
	entries, _ := h.stub.journal.since(0, JournalCapacity)
	return entries
}

// errorCode returns the Graph error code of a decoded error envelope.
func errorCode(t *testing.T, decoded map[string]any) int {
	t.Helper()
	envelope, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("no Graph error envelope in %v", decoded)
	}
	code, _ := envelope["code"].(float64)
	return int(code)
}

func dig(value any, path ...any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, _ := value.(map[string]any)
			value = object[key]
		case int:
			list, _ := value.([]any)
			if key >= len(list) {
				return nil
			}
			value = list[key]
		}
	}
	return value
}
