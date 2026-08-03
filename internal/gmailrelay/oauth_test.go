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
	deleteCalls  int
	deleteErr    error
	replaceCalls int
	replaceErr   error
	saveStarted  chan struct{}
	allowSave    chan struct{}
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
	if f.saveStarted != nil {
		close(f.saveStarted)
	}
	if f.allowSave != nil {
		<-f.allowSave
	}
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

func (f *fakeOAuthStore) ReplaceRefreshToken(
	_ context.Context,
	currentToken, replacementToken string,
) (bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.replaceCalls++
	if f.replaceErr != nil {
		return false, f.replaceErr
	}
	if f.refreshToken == "" || f.refreshToken != currentToken {
		return false, nil
	}
	f.refreshToken = replacementToken
	return true, nil
}

func (f *fakeOAuthStore) DeleteRefreshToken(context.Context) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.refreshToken = ""
	return nil
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

func TestAccessTokenRetainsStoredRefreshTokenWhenGoogleDoesNotRotate(t *testing.T) {
	var tokenMutex sync.Mutex
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			http.NotFound(response, request)
			return
		}
		tokenMutex.Lock()
		tokenCalls++
		tokenMutex.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"stable-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	store := newFakeOAuthStore()
	store.refreshToken = "stable-refresh-token"
	service := newTestOAuthService(t, server.URL, store, server.Client())
	first, err := service.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("first access token: %v", err)
	}
	second, err := service.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("cached access token: %v", err)
	}
	tokenMutex.Lock()
	calls := tokenCalls
	tokenMutex.Unlock()
	if first != "stable-access-token" || second != first || calls != 1 {
		t.Fatalf("stable token result: first=%q second=%q calls=%d", first, second, calls)
	}
	if service.cachedToken == nil || service.cachedToken.RefreshToken != "stable-refresh-token" {
		t.Fatalf("cached token lost shared refresh identity: %#v", service.cachedToken)
	}
}

func TestAccessTokenRechecksSharedCredentialBeforeUsingProcessCache(t *testing.T) {
	store := newFakeOAuthStore()
	store.refreshToken = "shared-refresh-token"
	service := newTestOAuthService(t, "", store, nil)
	service.cachedToken = &oauth2.Token{
		AccessToken:  "cached-access-token",
		RefreshToken: "shared-refresh-token",
		Expiry:       time.Now().UTC().Add(time.Hour),
	}

	accessToken, err := service.AccessToken(context.Background())
	if err != nil || accessToken != "cached-access-token" {
		t.Fatalf("use valid cache: access=%q err=%v", accessToken, err)
	}
	store.mutex.Lock()
	store.refreshToken = ""
	store.mutex.Unlock()
	if _, err := service.AccessToken(context.Background()); !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Fatalf("cross-replica credential deletion was not observed: %v", err)
	}
	if service.cachedToken != nil {
		t.Fatal("missing shared credential did not invalidate process cache")
	}
}

func TestAccessTokenRefreshesWhenSharedCredentialChanges(t *testing.T) {
	var tokenMutex sync.Mutex
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			http.NotFound(response, request)
			return
		}
		tokenMutex.Lock()
		tokenCalls++
		tokenMutex.Unlock()
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse refresh form: %v", err)
		}
		if request.Form.Get("refresh_token") != "replacement-refresh-token" {
			t.Errorf("refresh token = %q", request.Form.Get("refresh_token"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"replacement-access-token","refresh_token":"replacement-refresh-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	store := newFakeOAuthStore()
	store.refreshToken = "replacement-refresh-token"
	service := newTestOAuthService(t, server.URL, store, server.Client())
	service.cachedToken = &oauth2.Token{
		AccessToken:  "superseded-access-token",
		RefreshToken: "superseded-refresh-token",
		Expiry:       time.Now().UTC().Add(time.Hour),
	}

	accessToken, err := service.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("refresh replacement credential: %v", err)
	}
	tokenMutex.Lock()
	calls := tokenCalls
	tokenMutex.Unlock()
	if accessToken != "replacement-access-token" || calls != 1 {
		t.Fatalf("replacement token result: access=%q calls=%d", accessToken, calls)
	}
}

func TestAccessTokenCannotRestoreCredentialDeletedByAnotherReplica(t *testing.T) {
	refreshStarted := make(chan struct{})
	allowRefresh := make(chan struct{})
	releasedRefresh := false
	defer func() {
		if !releasedRefresh {
			close(allowRefresh)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			http.NotFound(response, request)
			return
		}
		close(refreshStarted)
		<-allowRefresh
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	store := newFakeOAuthStore()
	store.refreshToken = "current-refresh"
	refreshingReplica := newTestOAuthService(t, server.URL, store, server.Client())
	disconnectingReplica := newTestOAuthService(t, server.URL, store, server.Client())
	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := refreshingReplica.AccessToken(context.Background())
		refreshDone <- refreshErr
	}()
	<-refreshStarted
	if _, err := disconnectingReplica.Disconnect(context.Background()); err != nil {
		t.Fatalf("disconnect other replica: %v", err)
	}
	close(allowRefresh)
	releasedRefresh = true
	if err := <-refreshDone; !errors.Is(err, ErrAuthorizationChanged) {
		t.Fatalf("stale refresh result = %v, want authorization changed", err)
	}
	store.mutex.Lock()
	refreshToken := store.refreshToken
	store.mutex.Unlock()
	if refreshToken != "" || refreshingReplica.cachedToken != nil {
		t.Fatalf("stale refresh restored authorization: refresh=%q cached=%#v", refreshToken, refreshingReplica.cachedToken)
	}
}

func TestOAuthDisconnectDeletesConfiguredCredentialAndClearsCachedAccessToken(t *testing.T) {
	store := newFakeOAuthStore()
	store.refreshToken = "stored-refresh-token"
	service := newTestOAuthService(t, "", store, nil)
	service.cachedToken = &oauth2.Token{
		AccessToken: "short-lived-access-token",
		Expiry:      time.Now().UTC().Add(time.Hour),
	}

	result, err := service.Disconnect(context.Background())
	if err != nil {
		t.Fatalf("disconnect OAuth: %v", err)
	}
	if !result.Disconnected || result.Mailbox != service.config.Mailbox {
		t.Fatalf("disconnect result = %#v", result)
	}
	if service.cachedToken != nil {
		t.Fatal("disconnect left a cached access token")
	}
	secondResult, err := service.Disconnect(context.Background())
	if err != nil || !secondResult.Disconnected || secondResult.Mailbox != result.Mailbox {
		t.Fatalf("idempotent disconnect: result=%#v err=%v", secondResult, err)
	}
	store.mutex.Lock()
	deleteCalls := store.deleteCalls
	refreshToken := store.refreshToken
	store.mutex.Unlock()
	if deleteCalls != 2 || refreshToken != "" {
		t.Fatalf("disconnect store state: calls=%d refresh=%q", deleteCalls, refreshToken)
	}
	if _, err := service.AccessToken(context.Background()); !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Fatalf("access token remained usable after disconnect: %v", err)
	}
}

func TestOAuthDisconnectClearsCachedAccessTokenWhenStoreDeletionFails(t *testing.T) {
	store := newFakeOAuthStore()
	store.refreshToken = "stored-refresh-token"
	store.deleteErr = errors.New("redis unavailable")
	service := newTestOAuthService(t, "", store, nil)
	service.cachedToken = &oauth2.Token{
		AccessToken: "short-lived-access-token",
		Expiry:      time.Now().UTC().Add(time.Hour),
	}

	if _, err := service.Disconnect(context.Background()); err == nil {
		t.Fatal("disconnect succeeded despite store deletion failure")
	}
	if service.cachedToken != nil {
		t.Fatal("failed disconnect left a cached access token")
	}
}

func TestOAuthCallbackPersistenceIsSerializedAgainstDisconnect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/token":
			_, _ = response.Write([]byte(`{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			_, _ = response.Write([]byte(`{"emailAddress":"realignphysiolates@gmail.com"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	store := newFakeOAuthStore()
	store.saveStarted = make(chan struct{})
	store.allowSave = make(chan struct{})
	releasedSave := false
	defer func() {
		if !releasedSave {
			close(store.allowSave)
		}
	}()
	service := newTestOAuthService(t, server.URL, store, server.Client())
	start, err := service.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin OAuth: %v", err)
	}
	parsed, _ := url.Parse(start.AuthorizationURL)
	callbackDone := make(chan error, 1)
	go func() {
		_, completeErr := service.Complete(context.Background(), parsed.Query().Get("state"), "code")
		callbackDone <- completeErr
	}()
	<-store.saveStarted

	disconnectDone := make(chan error, 1)
	go func() {
		_, disconnectErr := service.Disconnect(context.Background())
		disconnectDone <- disconnectErr
	}()
	select {
	case disconnectErr := <-disconnectDone:
		t.Fatalf("disconnect returned before callback persistence completed: %v", disconnectErr)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.allowSave)
	releasedSave = true
	if err := <-callbackDone; err != nil {
		t.Fatalf("complete OAuth: %v", err)
	}
	if err := <-disconnectDone; err != nil {
		t.Fatalf("disconnect OAuth: %v", err)
	}
	store.mutex.Lock()
	refreshToken := store.refreshToken
	store.mutex.Unlock()
	if refreshToken != "" || service.cachedToken != nil {
		t.Fatalf("disconnect did not win after callback: refresh=%q cached=%#v", refreshToken, service.cachedToken)
	}
}
