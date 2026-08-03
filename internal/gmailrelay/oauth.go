package gmailrelay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	GmailReadonlyScope    = "https://www.googleapis.com/auth/gmail.readonly"
	GmailSendScope        = "https://www.googleapis.com/auth/gmail.send"
	maxOAuthResponseBytes = int64(1 << 20)
)

var (
	ErrAuthorizationDenied = errors.New("Google authorization was cancelled")
	ErrMailboxMismatch     = errors.New("authorized Google account does not match the configured mailbox")
	ErrRefreshTokenMissing = errors.New("Google did not issue a refresh token")
)

// OAuthStart is safe to return to the browser. The PKCE verifier remains only
// in the one-time Redis state.
type OAuthStart struct {
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// OAuthCallback contains the query parameters sent by Google's OAuth server.
// ProviderError is intentionally handled only after state has been consumed.
type OAuthCallback struct {
	State         string
	Code          string
	ProviderError string
}

// OAuthResult intentionally excludes both access and refresh tokens.
type OAuthResult struct {
	Mailbox string `json:"mailbox"`
}

type gmailProfile struct {
	EmailAddress string `json:"emailAddress"`
}

// OAuthService owns the complete Google authorization protocol for one
// configured mailbox.
type OAuthService struct {
	config      *Config
	store       OAuthStore
	client      *http.Client
	now         func() time.Time
	tokenMu     sync.Mutex
	cachedToken *oauth2.Token
}

// NewOAuthService creates an OAuth service. Store must be durable in
// production; RedisStore is the supported implementation.
func NewOAuthService(config *Config, store OAuthStore, client *http.Client) (*OAuthService, error) {
	if config == nil {
		return nil, errors.New("Gmail relay config is required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("Gmail OAuth store is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	if clientCopy.Timeout <= 0 {
		clientCopy.Timeout = config.HTTPTimeout
	}
	if clientCopy.CheckRedirect == nil {
		clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &OAuthService{
		config: config,
		store:  store,
		client: &clientCopy,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// Begin stores a single-use state for exactly ten minutes and builds a Google
// authorization request using PKCE S256 and offline access.
func (s *OAuthService) Begin(ctx context.Context) (OAuthStart, error) {
	if s == nil || s.config == nil || s.store == nil {
		return OAuthStart{}, errors.New("Gmail OAuth service is unavailable")
	}
	nonce, err := randomOAuthValue(32)
	if err != nil {
		return OAuthStart{}, errors.New("generate OAuth state")
	}
	codeVerifier := oauth2.GenerateVerifier()
	expiresAt := s.now().UTC().Add(OAuthStateTTL)
	state := OAuthState{
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		ExpiresAt:    expiresAt,
	}
	if err := s.store.SaveOAuthState(ctx, state); err != nil {
		return OAuthStart{}, err
	}
	authorizationURL := s.oauthConfig().AuthCodeURL(
		nonce,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(codeVerifier),
		oauth2.SetAuthURLParam("prompt", "consent select_account"),
	)
	return OAuthStart{AuthorizationURL: authorizationURL, ExpiresAt: expiresAt}, nil
}

// CompleteCallback atomically consumes callback state, exchanges the code,
// verifies /users/me/profile against the configured mailbox, and only then
// persists the refresh token.
func (s *OAuthService) CompleteCallback(ctx context.Context, callback OAuthCallback) (OAuthResult, error) {
	if s == nil || s.config == nil || s.store == nil {
		return OAuthResult{}, errors.New("Gmail OAuth service is unavailable")
	}
	stateNonce := strings.TrimSpace(callback.State)
	if !validOAuthOpaqueValue(stateNonce) {
		return OAuthResult{}, ErrOAuthStateNotFound
	}
	state, err := s.store.ConsumeOAuthState(ctx, stateNonce)
	if err != nil {
		return OAuthResult{}, err
	}
	if state.Nonce != stateNonce ||
		!validOAuthOpaqueValue(state.CodeVerifier) ||
		!s.now().UTC().Before(state.ExpiresAt) {
		return OAuthResult{}, ErrOAuthStateNotFound
	}
	if strings.TrimSpace(callback.ProviderError) != "" {
		return OAuthResult{}, ErrAuthorizationDenied
	}
	code := strings.TrimSpace(callback.Code)
	if code == "" || len(code) > 8192 {
		return OAuthResult{}, errors.New("Google authorization code is invalid")
	}

	exchangeContext := context.WithValue(ctx, oauth2.HTTPClient, s.client)
	token, err := s.oauthConfig().Exchange(
		exchangeContext,
		code,
		oauth2.VerifierOption(state.CodeVerifier),
	)
	if err != nil {
		return OAuthResult{}, errors.New("exchange Google authorization code")
	}
	refreshToken := strings.TrimSpace(token.RefreshToken)
	if refreshToken == "" {
		return OAuthResult{}, ErrRefreshTokenMissing
	}
	profile, err := s.fetchProfile(ctx, token)
	if err != nil {
		return OAuthResult{}, err
	}
	if strings.TrimSpace(profile.EmailAddress) != s.config.Mailbox {
		return OAuthResult{}, ErrMailboxMismatch
	}
	if err := s.store.SaveRefreshToken(ctx, refreshToken); err != nil {
		return OAuthResult{}, err
	}
	s.tokenMu.Lock()
	s.cachedToken = token
	s.tokenMu.Unlock()
	return OAuthResult{Mailbox: s.config.Mailbox}, nil
}

// Complete is the common success-callback shorthand.
func (s *OAuthService) Complete(ctx context.Context, state, code string) (OAuthResult, error) {
	return s.CompleteCallback(ctx, OAuthCallback{State: state, Code: code})
}

// AccessToken refreshes an access token from the encrypted token store without
// persisting the short-lived access token. A rotated refresh token is persisted
// through the same encrypted boundary.
func (s *OAuthService) AccessToken(ctx context.Context) (string, error) {
	if s == nil || s.config == nil || s.store == nil {
		return "", errors.New("Gmail OAuth service is unavailable")
	}
	// Serialize refreshes so a poll, health probe, and outbound send cannot
	// stampede Google's token endpoint. A one-minute margin prevents handing a
	// token to an operation that could expire while the request is in flight.
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.cachedToken != nil && strings.TrimSpace(s.cachedToken.AccessToken) != "" &&
		s.cachedToken.Expiry.After(s.now().UTC().Add(time.Minute)) {
		return s.cachedToken.AccessToken, nil
	}

	refreshToken, err := s.store.LoadRefreshToken(ctx)
	if err != nil {
		return "", err
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, s.client)
	seed := &oauth2.Token{RefreshToken: refreshToken}
	if s.cachedToken != nil {
		cachedCopy := *s.cachedToken
		seed = &cachedCopy
		// Redis is the source of truth after reconnecting the mailbox. Never
		// let a replica retain a superseded cached refresh token.
		seed.RefreshToken = refreshToken
	}
	token, err := s.oauthConfig().TokenSource(tokenContext, seed).Token()
	if err != nil || strings.TrimSpace(token.AccessToken) == "" {
		return "", errors.New("refresh Gmail access token")
	}
	if rotated := strings.TrimSpace(token.RefreshToken); rotated != "" && rotated != refreshToken {
		if err := s.store.SaveRefreshToken(ctx, rotated); err != nil {
			return "", err
		}
	}
	s.cachedToken = token
	return token.AccessToken, nil
}

func (s *OAuthService) fetchProfile(ctx context.Context, token *oauth2.Token) (gmailProfile, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(s.config.GmailAPIBaseURL, "/")+"/users/me/profile",
		nil,
	)
	if err != nil {
		return gmailProfile{}, errors.New("build Gmail profile request")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return gmailProfile{}, errors.New("fetch Gmail profile")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxOAuthResponseBytes))
		return gmailProfile{}, fmt.Errorf("Gmail profile returned HTTP %d", response.StatusCode)
	}
	var profile gmailProfile
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxOAuthResponseBytes))
	if err := decoder.Decode(&profile); err != nil || strings.TrimSpace(profile.EmailAddress) == "" {
		return gmailProfile{}, errors.New("Gmail profile response is invalid")
	}
	return profile, nil
}

func (s *OAuthService) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.config.GoogleClientID,
		ClientSecret: s.config.GoogleClientSecret,
		RedirectURL:  s.config.GoogleRedirectURL,
		Scopes:       []string{GmailReadonlyScope, GmailSendScope},
		Endpoint: oauth2.Endpoint{
			AuthURL:  s.config.GoogleAuthURL,
			TokenURL: s.config.GoogleTokenURL,
		},
	}
}

func randomOAuthValue(byteLength int) (string, error) {
	if byteLength <= 0 {
		return "", errors.New("random OAuth value length must be positive")
	}
	random := make([]byte, byteLength)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
