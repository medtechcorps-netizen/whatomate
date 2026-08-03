package gmailrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	serverTestMailbox        = "realignphysiolates@gmail.com"
	serverTestOutboundSecret = "outbound-signing-secret-at-least-32-bytes"
	serverTestSetupKey       = "setup-key-at-least-thirty-two-bytes-long"
)

type serverTestState struct {
	pingErr  error
	lastSync time.Time
}

func (s *serverTestState) Ping(context.Context) error { return s.pingErr }

func (s *serverTestState) GetLastSuccessfulSync(context.Context) (time.Time, error) {
	return s.lastSync, nil
}

type serverTestOAuth struct {
	start            OAuthStart
	startErr         error
	beginCalls       int
	disconnectResult OAuthDisconnectResult
	disconnectErr    error
	disconnectCalls  int
}

func (f *serverTestOAuth) Begin(context.Context) (OAuthStart, error) {
	f.beginCalls++
	return f.start, f.startErr
}

func (f *serverTestOAuth) CompleteCallback(context.Context, OAuthCallback) (OAuthResult, error) {
	return OAuthResult{Mailbox: serverTestMailbox}, nil
}

func (f *serverTestOAuth) Disconnect(context.Context) (OAuthDisconnectResult, error) {
	f.disconnectCalls++
	return f.disconnectResult, f.disconnectErr
}

type serverTestGmail struct {
	mu sync.Mutex

	profile GmailProfile
	thread  GmailThread

	searchResults []GmailMessageList
	searchErrors  []error
	searchCalls   int
	searchQueries []string

	sendResponse GmailSendResponse
	sendErr      error
	sendCalls    int
	sentThreads  []string
	sentRaw      [][]byte
	getCalls     int
}

func (f *serverTestGmail) Profile(context.Context) (GmailProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profile, nil
}

func (f *serverTestGmail) GetThread(context.Context, string) (GmailThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.thread, nil
}

func (f *serverTestGmail) SearchMessages(_ context.Context, query string, _ int) (GmailMessageList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.searchCalls
	f.searchCalls++
	f.searchQueries = append(f.searchQueries, query)
	if index < len(f.searchErrors) && f.searchErrors[index] != nil {
		return GmailMessageList{}, f.searchErrors[index]
	}
	if index < len(f.searchResults) {
		return f.searchResults[index], nil
	}
	return GmailMessageList{}, nil
}

func (f *serverTestGmail) SendRaw(_ context.Context, threadID string, raw []byte) (GmailSendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls++
	f.sentThreads = append(f.sentThreads, threadID)
	f.sentRaw = append(f.sentRaw, append([]byte(nil), raw...))
	return f.sendResponse, f.sendErr
}

type serverTestOutboundRecord struct {
	digest string
	state  metarelay.OutboundClaimState
	status int
	result []byte
}

type serverTestOutboundStore struct {
	mu sync.Mutex

	records map[string]*serverTestOutboundRecord

	claimCalls     int
	completeCalls  int
	releaseCalls   int
	ambiguousCalls int
	resolveCalls   int
}

func newServerTestOutboundStore() *serverTestOutboundStore {
	return &serverTestOutboundStore{records: make(map[string]*serverTestOutboundRecord)}
}

func (s *serverTestOutboundStore) ClaimOutbound(
	_ context.Context,
	key, digest string,
	_ time.Time,
) (metarelay.OutboundClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	record, exists := s.records[key]
	if !exists {
		s.records[key] = &serverTestOutboundRecord{
			digest: digest,
			state:  metarelay.OutboundClaimInFlight,
		}
		return metarelay.OutboundClaim{State: metarelay.OutboundClaimAcquired}, nil
	}
	if record.digest != digest {
		return metarelay.OutboundClaim{State: metarelay.OutboundClaimCollision}, nil
	}
	return metarelay.OutboundClaim{
		State:  record.state,
		Status: record.status,
		Result: append([]byte(nil), record.result...),
	}, nil
}

func (s *serverTestOutboundStore) CompleteOutbound(
	_ context.Context,
	key, digest string,
	status int,
	result []byte,
	rejected bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	record, exists := s.records[key]
	if !exists || record.digest != digest || record.state != metarelay.OutboundClaimInFlight {
		return errors.New("invalid completion")
	}
	record.state = metarelay.OutboundClaimCompleted
	if rejected {
		record.state = metarelay.OutboundClaimRejected
	}
	record.status = status
	record.result = append([]byte(nil), result...)
	return nil
}

func (s *serverTestOutboundStore) ReleaseOutbound(_ context.Context, key, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	if record, exists := s.records[key]; exists && record.digest == digest {
		delete(s.records, key)
	}
	return nil
}

func (s *serverTestOutboundStore) MarkOutboundAmbiguous(_ context.Context, key, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ambiguousCalls++
	record, exists := s.records[key]
	if !exists || record.digest != digest || record.state != metarelay.OutboundClaimInFlight {
		return errors.New("invalid ambiguous transition")
	}
	record.state = metarelay.OutboundClaimAmbiguous
	return nil
}

func (s *serverTestOutboundStore) ResolveOutboundAmbiguous(
	_ context.Context,
	key, digest string,
	status int,
	result []byte,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveCalls++
	record, exists := s.records[key]
	if !exists || record.digest != digest || record.state != metarelay.OutboundClaimAmbiguous {
		return errors.New("invalid reconciliation")
	}
	record.state = metarelay.OutboundClaimCompleted
	record.status = status
	record.result = append([]byte(nil), result...)
	return nil
}

type serverTestHarness struct {
	config   *Config
	state    *serverTestState
	oauth    *serverTestOAuth
	gmail    *serverTestGmail
	outbound *serverTestOutboundStore
	handler  http.Handler
}

func newServerTestHarness(t *testing.T) *serverTestHarness {
	t.Helper()
	config := &Config{
		Mailbox:               serverTestMailbox,
		GoogleClientSecret:    "google-client-secret-must-not-leak",
		EncryptionKey:         "encryption-key-must-not-leak-and-is-long-enough",
		SetupKey:              serverTestSetupKey,
		ReReplyWebhookURL:     "https://app.rereply.app/api/webhooks/channels/account",
		ReReplyInboundSecret:  "inbound-signing-secret-at-least-32-bytes",
		ReReplyOutboundSecret: serverTestOutboundSecret,
		HTTPTimeout:           2 * time.Second,
		GmailPollInterval:     time.Minute,
	}
	state := &serverTestState{lastSync: time.Date(2026, time.August, 3, 1, 0, 0, 0, time.UTC)}
	oauth := &serverTestOAuth{
		start: OAuthStart{
			AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth?client_id=public-client-id&state=opaque-state",
			ExpiresAt:        time.Date(2026, time.August, 3, 1, 10, 0, 0, time.UTC),
		},
		disconnectResult: OAuthDisconnectResult{Mailbox: serverTestMailbox, Disconnected: true},
	}
	gmail := &serverTestGmail{
		profile:      GmailProfile{EmailAddress: serverTestMailbox, HistoryID: "101"},
		thread:       serverTestThread(),
		sendResponse: GmailSendResponse{ID: "sent-message-1", ThreadID: "thread-1"},
	}
	outbound := newServerTestOutboundStore()
	server, err := NewServer(config, state, oauth, gmail, outbound)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.now = func() time.Time {
		return time.Date(2026, time.August, 3, 1, 0, 0, 0, time.UTC)
	}
	return &serverTestHarness{
		config:   config,
		state:    state,
		oauth:    oauth,
		gmail:    gmail,
		outbound: outbound,
		handler:  server.Handler(),
	}
}

func serverTestThread() GmailThread {
	text := []byte("Customer's original message")
	return GmailThread{
		ID: "thread-1",
		Messages: []GmailRESTMessage{{
			ID:           "incoming-message-1",
			ThreadID:     "thread-1",
			InternalDate: "1785720000000",
			Payload: &GmailRESTPart{
				MimeType: "text/plain; charset=UTF-8",
				Headers: []GmailRESTHeader{
					{Name: "From", Value: "Customer <customer@example.com>"},
					{Name: "Subject", Value: "Physio appointment"},
					{Name: "Message-ID", Value: "<incoming-message-1@example.com>"},
				},
				Body: GmailRESTBody{Data: EncodeBase64URL(text), Size: int64(len(text))},
			},
		}},
	}
}

func serverTestOutboundBody(t *testing.T, idempotencyKey, text string) []byte {
	t.Helper()
	payload := struct {
		Type              string                     `json:"type"`
		Channel           models.Channel             `json:"channel"`
		ExternalAccountID string                     `json:"external_account_id"`
		Data              channelapi.OutboundMessage `json:"data"`
	}{
		Type:              "message",
		Channel:           models.ChannelEmail,
		ExternalAccountID: serverTestMailbox,
		Data: channelapi.OutboundMessage{
			IdempotencyKey: idempotencyKey,
			Conversation:   channelapi.ConversationRef{ExternalID: "thread-1"},
			Recipient:      channelapi.Participant{Address: "customer@example.com"},
			Parts: []channelapi.MessagePart{{
				Type: models.MessagePartTypeText,
				Text: text,
			}},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal outbound payload: %v", err)
	}
	return raw
}

func serverTestRequest(handler http.Handler, method, path string, raw []byte, signature string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if signature != "" {
		request.Header.Set(ReReplySignatureHeader, signature)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestServerOutboundHMACAuthenticatesExactBodyBytes(t *testing.T) {
	harness := newServerTestHarness(t)
	compact := []byte(`{"type":"read","channel":"email","external_account_id":"realignphysiolates@gmail.com"}`)
	withWhitespace := append(append([]byte(nil), compact...), '\n')
	path := "/v1/accounts/email/" + serverTestMailbox

	altered := serverTestRequest(
		harness.handler,
		http.MethodPost,
		path,
		withWhitespace,
		signRelayBody(serverTestOutboundSecret, compact),
	)
	if altered.Code != http.StatusUnauthorized {
		t.Fatalf("altered-body status = %d, want %d; body = %s", altered.Code, http.StatusUnauthorized, altered.Body.String())
	}

	exact := serverTestRequest(
		harness.handler,
		http.MethodPost,
		path,
		withWhitespace,
		signRelayBody(serverTestOutboundSecret, withWhitespace),
	)
	if exact.Code != http.StatusNoContent {
		t.Fatalf("exact-body status = %d, want %d; body = %s", exact.Code, http.StatusNoContent, exact.Body.String())
	}
}

func TestServerAccountHealthRequiresEmptyBodySignature(t *testing.T) {
	harness := newServerTestHarness(t)
	path := "/v1/accounts/email/" + serverTestMailbox

	wrong := serverTestRequest(
		harness.handler,
		http.MethodHead,
		path,
		nil,
		signRelayBody(serverTestOutboundSecret, []byte("not-empty")),
	)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-signature status = %d, want %d", wrong.Code, http.StatusUnauthorized)
	}

	valid := serverTestRequest(
		harness.handler,
		http.MethodHead,
		path,
		nil,
		signRelayBody(serverTestOutboundSecret, nil),
	)
	if valid.Code != http.StatusNoContent {
		t.Fatalf("empty-body signature status = %d, want %d; body = %s", valid.Code, http.StatusNoContent, valid.Body.String())
	}
}

func TestServerOAuthStartRequiresSetupKeyAndDoesNotLeakSecrets(t *testing.T) {
	harness := newServerTestHarness(t)

	unauthorized := serverTestRequest(harness.handler, http.MethodPost, "/oauth/google/start", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing setup-key status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if harness.oauth.beginCalls != 0 {
		t.Fatalf("Begin() calls after missing setup key = %d, want 0", harness.oauth.beginCalls)
	}

	request := httptest.NewRequest(http.MethodPost, "/oauth/google/start", nil)
	request.Header.Set(SetupKeyHeader, serverTestSetupKey)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid setup-key status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if harness.oauth.beginCalls != 1 {
		t.Fatalf("Begin() calls = %d, want 1", harness.oauth.beginCalls)
	}
	for _, secret := range []string{
		harness.config.GoogleClientSecret,
		harness.config.EncryptionKey,
		harness.config.SetupKey,
		harness.config.ReReplyInboundSecret,
		harness.config.ReReplyOutboundSecret,
	} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("OAuth start response leaked configured secret %q", secret)
		}
	}
	if !strings.Contains(response.Body.String(), "authorization_url") ||
		!strings.Contains(response.Body.String(), "public-client-id") {
		t.Fatalf("OAuth start response did not contain the public authorization URL: %s", response.Body.String())
	}
}

func TestServerOAuthDisconnectRequiresSetupKeyAndReturnsOnlySafeConfirmation(t *testing.T) {
	harness := newServerTestHarness(t)
	path := "/oauth/google/disconnect"

	unauthorized := serverTestRequest(harness.handler, http.MethodPost, path, nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing setup-key status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if harness.oauth.disconnectCalls != 0 {
		t.Fatalf("Disconnect() calls after missing setup key = %d, want 0", harness.oauth.disconnectCalls)
	}
	withMailboxBody := httptest.NewRequest(
		http.MethodPost,
		path,
		strings.NewReader(`{"mailbox":"other@gmail.com"}`),
	)
	withMailboxBody.Header.Set(SetupKeyHeader, serverTestSetupKey)
	invalidResponse := httptest.NewRecorder()
	harness.handler.ServeHTTP(invalidResponse, withMailboxBody)
	if invalidResponse.Code != http.StatusBadRequest || harness.oauth.disconnectCalls != 0 {
		t.Fatalf(
			"mailbox-bearing request = %d with %d calls; want 400 with 0 calls",
			invalidResponse.Code,
			harness.oauth.disconnectCalls,
		)
	}

	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.Header.Set(SetupKeyHeader, serverTestSetupKey)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid setup-key status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if harness.oauth.disconnectCalls != 1 {
		t.Fatalf("Disconnect() calls = %d, want 1", harness.oauth.disconnectCalls)
	}
	var result OAuthDisconnectResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode disconnect response: %v", err)
	}
	if !result.Disconnected || result.Mailbox != serverTestMailbox {
		t.Fatalf("disconnect response = %#v", result)
	}
	for _, secret := range []string{
		harness.config.GoogleClientSecret,
		harness.config.EncryptionKey,
		harness.config.SetupKey,
		harness.config.ReReplyInboundSecret,
		harness.config.ReReplyOutboundSecret,
	} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("OAuth disconnect response leaked configured secret %q", secret)
		}
	}
	for header, expected := range map[string]string{
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	} {
		if value := response.Header().Get(header); value != expected {
			t.Fatalf("disconnect %s = %q, want %q", header, value, expected)
		}
	}
}

func TestServerOAuthDisconnectFailureIsGenericAndDoesNotLeakProviderDetails(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.oauth.disconnectErr = errors.New("failed to delete refresh token secret-provider-detail")
	request := httptest.NewRequest(http.MethodPost, "/oauth/google/disconnect", nil)
	request.Header.Set(SetupKeyHeader, serverTestSetupKey)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "oauth_disconnect_failed") {
		t.Fatalf("disconnect failure response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret-provider-detail") {
		t.Fatalf("disconnect failure leaked internal details: %s", response.Body.String())
	}
}

func TestServerOutboundRejectsUnauthenticatedAndMismatchedEnvelopesBeforeProviderUse(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]any)
		sign       bool
		wantStatus int
	}{
		{name: "wrong signature", sign: false, wantStatus: http.StatusUnauthorized},
		{
			name: "mailbox mismatch",
			mutate: func(envelope map[string]any) {
				envelope["external_account_id"] = "other@gmail.com"
			},
			sign: true, wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "channel mismatch",
			mutate: func(envelope map[string]any) {
				envelope["channel"] = "whatsapp"
			},
			sign: true, wantStatus: http.StatusUnprocessableEntity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newServerTestHarness(t)
			base := serverTestOutboundBody(t, "delivery-auth-check", "Reply")
			var envelope map[string]any
			if err := json.Unmarshal(base, &envelope); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if test.mutate != nil {
				test.mutate(envelope)
			}
			raw, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("encode fixture: %v", err)
			}
			signature := signRelayBody(serverTestOutboundSecret, []byte("different body"))
			if test.sign {
				signature = signRelayBody(serverTestOutboundSecret, raw)
			}
			response := serverTestRequest(
				harness.handler,
				http.MethodPost,
				"/v1/accounts/email/"+serverTestMailbox,
				raw,
				signature,
			)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			if harness.outbound.claimCalls != 0 || harness.gmail.getCalls != 0 || harness.gmail.sendCalls != 0 {
				t.Fatalf(
					"rejected request touched delivery dependencies: claims=%d gets=%d sends=%d",
					harness.outbound.claimCalls,
					harness.gmail.getCalls,
					harness.gmail.sendCalls,
				)
			}
		})
	}
}

func TestServerDuplicateIdempotencyKeyPerformsOneProviderSend(t *testing.T) {
	harness := newServerTestHarness(t)
	raw := serverTestOutboundBody(t, "delivery-duplicate-1", "Your appointment is confirmed.")
	signature := signRelayBody(serverTestOutboundSecret, raw)
	path := "/v1/accounts/email/" + serverTestMailbox

	first := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	second := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("duplicate statuses = %d and %d; bodies = %q / %q", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached duplicate response differs: %q != %q", first.Body.String(), second.Body.String())
	}
	if harness.gmail.sendCalls != 1 || harness.gmail.getCalls != 1 || harness.gmail.searchCalls != 1 {
		t.Fatalf(
			"provider calls = send %d, thread %d, search %d; want exactly one each",
			harness.gmail.sendCalls,
			harness.gmail.getCalls,
			harness.gmail.searchCalls,
		)
	}
	if harness.outbound.claimCalls != 2 || harness.outbound.completeCalls != 1 {
		t.Fatalf("store calls = claim %d, complete %d; want 2 and 1", harness.outbound.claimCalls, harness.outbound.completeCalls)
	}
	if len(harness.gmail.sentRaw) != 1 || !bytes.Contains(harness.gmail.sentRaw[0], []byte("Message-ID:")) {
		t.Fatalf("provider did not receive exactly one RFC message with a Message-ID")
	}
}

func TestServerRejectsIdempotencyCollisionWithoutSecondProviderSend(t *testing.T) {
	harness := newServerTestHarness(t)
	path := "/v1/accounts/email/" + serverTestMailbox
	firstRaw := serverTestOutboundBody(t, "delivery-collision-1", "First version")
	secondRaw := serverTestOutboundBody(t, "delivery-collision-1", "Different version")

	first := serverTestRequest(
		harness.handler,
		http.MethodPost,
		path,
		firstRaw,
		signRelayBody(serverTestOutboundSecret, firstRaw),
	)
	second := serverTestRequest(
		harness.handler,
		http.MethodPost,
		path,
		secondRaw,
		signRelayBody(serverTestOutboundSecret, secondRaw),
	)
	if first.Code != http.StatusOK || second.Code != http.StatusConflict {
		t.Fatalf("collision statuses = %d and %d; second body = %s", first.Code, second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "idempotency_key_collision") {
		t.Fatalf("collision response = %s", second.Body.String())
	}
	if harness.gmail.sendCalls != 1 {
		t.Fatalf("provider send calls = %d, want 1", harness.gmail.sendCalls)
	}
}

func TestServerAmbiguousSendReconcilesWithoutResending(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.gmail.searchResults = []GmailMessageList{
		{},
		{Messages: []GmailMessageRef{{ID: "sent-message-reconciled", ThreadID: "thread-1"}}},
	}
	harness.gmail.sendErr = errors.New("provider response was lost after request write")
	raw := serverTestOutboundBody(t, "delivery-ambiguous-1", "This must be sent once.")
	signature := signRelayBody(serverTestOutboundSecret, raw)
	path := "/v1/accounts/email/" + serverTestMailbox

	first := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "delivery_outcome_ambiguous") {
		t.Fatalf("ambiguous first response = %d %s", first.Code, first.Body.String())
	}
	if first.Header().Get("Retry-After") != "5" {
		t.Fatalf("ambiguous first Retry-After = %q, want 5", first.Header().Get("Retry-After"))
	}
	if harness.outbound.ambiguousCalls != 1 {
		t.Fatalf("MarkOutboundAmbiguous() calls = %d, want 1", harness.outbound.ambiguousCalls)
	}

	second := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	if second.Code != http.StatusOK {
		t.Fatalf("reconciled response = %d %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "sent-message-reconciled") {
		t.Fatalf("reconciled response does not use discovered Gmail message: %s", second.Body.String())
	}
	if harness.outbound.resolveCalls != 1 {
		t.Fatalf("ResolveOutboundAmbiguous() calls = %d, want 1", harness.outbound.resolveCalls)
	}
	if harness.gmail.sendCalls != 1 {
		t.Fatalf("provider send calls after reconciliation = %d, want 1", harness.gmail.sendCalls)
	}

	third := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	if third.Code != http.StatusOK || third.Body.String() != second.Body.String() {
		t.Fatalf("cached reconciled response = %d %q; want 200 %q", third.Code, third.Body.String(), second.Body.String())
	}
	if harness.gmail.sendCalls != 1 || harness.gmail.searchCalls != 2 {
		t.Fatalf("provider calls after cached replay = sends %d, searches %d; want 1 and 2", harness.gmail.sendCalls, harness.gmail.searchCalls)
	}
}

func TestServerAmbiguousReplayWithoutEvidenceNeverResends(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.gmail.searchResults = []GmailMessageList{{}, {}}
	harness.gmail.sendErr = io.ErrUnexpectedEOF
	raw := serverTestOutboundBody(t, "delivery-ambiguous-unresolved", "Send no more than once.")
	signature := signRelayBody(serverTestOutboundSecret, raw)
	path := "/v1/accounts/email/" + serverTestMailbox

	first := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	second := serverTestRequest(harness.handler, http.MethodPost, path, raw, signature)
	if first.Code != http.StatusServiceUnavailable || second.Code != http.StatusServiceUnavailable {
		t.Fatalf("unresolved ambiguous statuses = %d and %d; bodies = %q / %q", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if first.Header().Get("Retry-After") != "5" || second.Header().Get("Retry-After") != "5" {
		t.Fatalf(
			"unresolved ambiguous Retry-After headers = %q and %q, want 5",
			first.Header().Get("Retry-After"),
			second.Header().Get("Retry-After"),
		)
	}
	if harness.gmail.sendCalls != 1 {
		t.Fatalf("provider send calls = %d, want 1", harness.gmail.sendCalls)
	}
	if harness.outbound.resolveCalls != 0 {
		t.Fatalf("ResolveOutboundAmbiguous() calls = %d, want 0", harness.outbound.resolveCalls)
	}
}
