package gmailrelay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type fakeOAuthStore struct {
	mutex        sync.Mutex
	states       map[string]OAuthState
	savedTokens  []string
	refreshToken string
}

func newFakeOAuthStore() *fakeOAuthStore {
	return &fakeOAuthStore{states: make(map[string]OAuthState)}
}

func (f *fakeOAuthStore) SaveOAuthState(_ context.Context, state OAuthState) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if _, exists := f.states[state.Nonce]; exists {
		return ErrOAuthStateCollision
	}
	f.states[state.Nonce] = state
	return nil
}

func (f *fakeOAuthStore) ConsumeOAuthState(_ context.Context, nonce string) (OAuthState, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	state, exists := f.states[nonce]
	if !exists {
		return OAuthState{}, ErrOAuthStateNotFound
	}
	delete(f.states, nonce)
	return state, nil
}

func (f *fakeOAuthStore) SaveRefreshToken(_ context.Context, refreshToken string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.refreshToken = refreshToken
	f.savedTokens = append(f.savedTokens, refreshToken)
	return nil
}

func (f *fakeOAuthStore) LoadRefreshToken(context.Context) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.refreshToken == "" {
		return "", ErrRefreshTokenNotFound
	}
	return f.refreshToken, nil
}

func newTestOAuthService(t *testing.T, serverURL string, store *fakeOAuthStore, client *http.Client) *OAuthService {
	t.Helper()
	config, err := loadTestConfig(validConfigEnvironment())
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	if serverURL != "" {
		config.GoogleAuthURL = serverURL + "/auth"
		config.GoogleTokenURL = serverURL + "/token"
		config.GmailAPIBaseURL = serverURL + "/gmail/v1"
		config.GoogleRedirectURL = serverURL + "/oauth/google/callback"
	}
	service, err := NewOAuthService(config, store, client)
	if err != nil {
		t.Fatalf("create OAuth service: %v", err)
	}
	return service
}

func TestOAuthBeginUsesExactScopesOfflineConsentAndPKCES256(t *testing.T) {
	store := newFakeOAuthStore()
	service := newTestOAuthService(t, "", store, nil)
	fixedNow := time.Date(2026, 8, 3, 1, 2, 3, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	start, err := service.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin OAuth: %v", err)
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("scope") != GmailReadonlyScope+" "+GmailSendScope {
		t.Fatalf("unexpected scopes %q", query.Get("scope"))
	}
	if query.Get("access_type") != "offline" {
		t.Fatalf("access_type = %q", query.Get("access_type"))
	}
	if query.Get("prompt") != "consent select_account" {
		t.Fatalf("prompt = %q", query.Get("prompt"))
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatal("PKCE S256 challenge is missing")
	}
	if query.Get("include_granted_scopes") != "" {
		t.Fatal("incremental authorization must not broaden the exact scope set")
	}
	if query.Get("state") == "" || start.ExpiresAt != fixedNow.Add(10*time.Minute) {
		t.Fatal("state or ten-minute expiry is invalid")
	}

	store.mutex.Lock()
	state := store.states[query.Get("state")]
	store.mutex.Unlock()
	if state.Nonce != query.Get("state") || state.ExpiresAt != start.ExpiresAt {
		t.Fatalf("stored state mismatch: %#v", state)
	}
	if query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(state.CodeVerifier) {
		t.Fatal("authorization challenge does not match stored verifier")
	}
	if strings.Contains(start.AuthorizationURL, state.CodeVerifier) {
		t.Fatal("PKCE verifier leaked into the authorization URL")
	}
}

func TestOAuthCallbackVerifiesExactMailboxBeforeSavingRefreshToken(t *testing.T) {
	var mutex sync.Mutex
	tokenRequests := 0
	profileRequests := 0
	expectedVerifier := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Errorf("parse token request: %v", err)
			}
			mutex.Lock()
			tokenRequests++
			mutex.Unlock()
			if request.Form.Get("code") != "authorization-code" ||
				request.Form.Get("code_verifier") != expectedVerifier ||
				request.Form.Get("grant_type") != "authorization_code" {
				t.Errorf("unexpected token form: %v", request.Form)
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				"access_token":  "short-lived-access-token",
				"refresh_token": "long-lived-refresh-token",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case "/gmail/v1/users/me/profile":
			mutex.Lock()
			profileRequests++
			mutex.Unlock()
			if request.Header.Get("Authorization") != "Bearer short-lived-access-token" {
				t.Errorf("unexpected profile authorization header %q", request.Header.Get("Authorization"))
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"emailAddress":"realignphysiolates@gmail.com","messagesTotal":10}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	store := newFakeOAuthStore()
	service := newTestOAuthService(t, server.URL, store, server.Client())
	start, err := service.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin OAuth: %v", err)
	}
	parsed, _ := url.Parse(start.AuthorizationURL)
	stateNonce := parsed.Query().Get("state")
	store.mutex.Lock()
	expectedVerifier = store.states[stateNonce].CodeVerifier
	store.mutex.Unlock()

	result, err := service.Complete(context.Background(), stateNonce, "authorization-code")
	if err != nil {
		t.Fatalf("complete OAuth: %v", err)
	}
	if result.Mailbox != "realignphysiolates@gmail.com" {
		t.Fatalf("unexpected result %#v", result)
	}
	store.mutex.Lock()
	savedTokens := append([]string(nil), store.savedTokens...)
	store.mutex.Unlock()
	if len(savedTokens) != 1 || savedTokens[0] != "long-lived-refresh-token" {
		t.Fatalf("unexpected persisted credentials %#v", savedTokens)
	}
	mutex.Lock()
	if tokenRequests != 1 || profileRequests != 1 {
		t.Fatalf("unexpected request counts token=%d profile=%d", tokenRequests, profileRequests)
	}
	mutex.Unlock()

	if _, err := service.Complete(context.Background(), stateNonce, "replay-code"); !errors.Is(err, ErrOAuthStateNotFound) {
		t.Fatalf("callback replay should fail, got %v", err)
	}
	mutex.Lock()
	if tokenRequests != 1 {
		t.Fatalf("replay reached token endpoint: %d requests", tokenRequests)
	}
	mutex.Unlock()
}

func TestOAuthCallbackRejectsDifferentOrNonExactMailboxWithoutWritingToken(t *testing.T) {
	for _, profileMailbox := range []string{"other@gmail.com", "Realignphysiolates@gmail.com"} {
		t.Run(profileMailbox, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/token":
					_, _ = response.Write([]byte(`{"access_token":"access","refresh_token":"must-not-save","token_type":"Bearer"}`))
				case "/gmail/v1/users/me/profile":
					_ = json.NewEncoder(response).Encode(map[string]string{"emailAddress": profileMailbox})
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			store := newFakeOAuthStore()
			service := newTestOAuthService(t, server.URL, store, server.Client())
			start, err := service.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin OAuth: %v", err)
			}
			parsed, _ := url.Parse(start.AuthorizationURL)
			_, err = service.Complete(context.Background(), parsed.Query().Get("state"), "code")
			if !errors.Is(err, ErrMailboxMismatch) {
				t.Fatalf("expected exact mailbox mismatch, got %v", err)
			}
			if len(store.savedTokens) != 0 {
				t.Fatalf("mismatched account token was persisted: %#v", store.savedTokens)
			}
		})
	}
}

func TestOAuthCancellationConsumesStateBeforeReturning(t *testing.T) {
	store := newFakeOAuthStore()
	service := newTestOAuthService(t, "", store, nil)
	start, err := service.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin OAuth: %v", err)
	}
	parsed, _ := url.Parse(start.AuthorizationURL)
	stateNonce := parsed.Query().Get("state")
	_, err = service.CompleteCallback(context.Background(), OAuthCallback{
		State:         stateNonce,
		ProviderError: "access_denied",
	})
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if _, err := service.Complete(context.Background(), stateNonce, "code"); !errors.Is(err, ErrOAuthStateNotFound) {
		t.Fatalf("cancelled callback state should be consumed, got %v", err)
	}
}

func TestOAuthCallbackRequiresRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/token" {
			_, _ = response.Write([]byte(`{"access_token":"access-only","token_type":"Bearer"}`))
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	store := newFakeOAuthStore()
	service := newTestOAuthService(t, server.URL, store, server.Client())
	start, err := service.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin OAuth: %v", err)
	}
	parsed, _ := url.Parse(start.AuthorizationURL)
	_, err = service.Complete(context.Background(), parsed.Query().Get("state"), "code")
	if !errors.Is(err, ErrRefreshTokenMissing) {
		t.Fatalf("expected refresh token requirement, got %v", err)
	}
	if len(store.savedTokens) != 0 {
		t.Fatal("an incomplete grant was persisted")
	}
}

func TestAccessTokenUsesStoredRefreshTokenAndPersistsRotation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			http.NotFound(response, request)
			return
		}
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse refresh form: %v", err)
		}
		if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "stored-refresh" {
			t.Errorf("unexpected refresh form: %v", request.Form)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	store := newFakeOAuthStore()
	store.refreshToken = "stored-refresh"
	service := newTestOAuthService(t, server.URL, store, server.Client())
	accessToken, err := service.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("refresh access token: %v", err)
	}
	if accessToken != "new-access" || store.refreshToken != "rotated-refresh" {
		t.Fatalf("unexpected token result access=%q refresh=%q", accessToken, store.refreshToken)
	}
}
