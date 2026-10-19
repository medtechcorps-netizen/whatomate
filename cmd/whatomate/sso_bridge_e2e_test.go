package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// The real-browser, two-callback SSO proof (R1).
//
// TestSSOBridgeRealBrowserTwoCallbacks serves the real application (the
// production middleware chain and setupRoutes, with the embedded frontend)
// on a private loopback TLS listener, over a test database and Redis, with
// two organizations whose "custom" SSO providers are a local fake. It then
// runs one focused Playwright case (frontend/e2e/sso-bridge) in a real
// browser: two pages of one browser context sign in through the real
// InitSSO, CallbackSSO and session exchange, and the case checks the order
// the browser imposes (Web Locks, the binding cookie of each attempt, the
// cookies each answer sets).
//
// The identity provider is the only fake. Its authorization page, token and
// userinfo endpoints are the TLS server below. The browser reaches its made-up
// hosts through Chromium's --host-resolver-rules (the real InitSSO redirect is
// followed as is), and its authorization page sends the browser back to the
// exact redirect URI with the real state by a script navigation (so the
// browser case can hold the callback; Playwright does not route requests a
// redirect leads to). The real CallbackSSO calls its token and userinfo
// endpoints through an HTTP client mapped to these two origins only.
//
// TLS: with Environment "test" the server builds an https callback URL, so
// the app is served over https (a generated loopback certificate) to keep
// that redirect URI exact. Cookies are Secure, as in production.
//
// Opt-in only. It needs TEST_DATABASE_URL, TEST_REDIS_URL, node and the
// frontend's npm dependencies, a Chromium for Playwright 1.57, and the
// candidate frontend built and embedded before this test binary is compiled:
//
//	cd frontend && npm ci && npm run build && cd ..
//	rm -rf internal/frontend/dist/* && cp -r frontend/dist/* internal/frontend/dist/
//	REREPLY_E2E_SSO_BRIDGE=1 TEST_DATABASE_URL=... TEST_REDIS_URL=... \
//	  go test -count=1 -run '^TestSSOBridgeRealBrowserTwoCallbacks$' -v ./cmd/whatomate/
//
// Once opted in, every missing prerequisite fails the test; nothing skips.

const (
	ssoBridgeOptInEnv      = "REREPLY_E2E_SSO_BRIDGE"
	ssoBridgeEncryptionKey = "sso-bridge-encryption-key-long-enough"
	ssoBridgeProviderA     = "https://idp-a.example.test"
	ssoBridgeProviderB     = "https://idp-b.example.test"
	ssoBridgeClientA       = "lane-e-bridge-client-a"
	ssoBridgeClientB       = "lane-e-bridge-client-b"
	ssoBridgeCodeA         = "lane-e-bridge-code-a"
	ssoBridgeCodeB         = "lane-e-bridge-code-b"
	ssoBridgeTokenA        = "lane-e-bridge-token-a"
	ssoBridgeTokenB        = "lane-e-bridge-token-b"
	ssoBridgeTestTitle     = "two SSO callbacks in one browser"
	ssoBridgeBrowserBudget = 4 * time.Minute
)

// ssoBridgeIdentity is what the fake provider's userinfo answers for one
// access token.
type ssoBridgeIdentity struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
}

// ssoBridgeProvider is a local OAuth provider (authorization page, token and
// userinfo) with a fixed host -> code, code -> client, code -> token and
// token -> identity mapping, set before the app starts and never changed (no
// shared "current user" field).
type ssoBridgeProvider struct {
	server *httptest.Server

	codes   map[string]string
	clients map[string]string
	tokens  map[string]string
	people  map[string]ssoBridgeIdentity

	mu             sync.Mutex
	redirectURI    string
	authorizeCalls map[string]int
	refused        []string
	tokenCalls     map[string]int
	userinfoCalls  map[string]int
}

func newSSOBridgeProvider(t *testing.T, people map[string]ssoBridgeIdentity) *ssoBridgeProvider {
	t.Helper()
	provider := &ssoBridgeProvider{
		codes: map[string]string{
			strings.TrimPrefix(ssoBridgeProviderA, "https://"): ssoBridgeCodeA,
			strings.TrimPrefix(ssoBridgeProviderB, "https://"): ssoBridgeCodeB,
		},
		clients:        map[string]string{ssoBridgeCodeA: ssoBridgeClientA, ssoBridgeCodeB: ssoBridgeClientB},
		tokens:         map[string]string{ssoBridgeCodeA: ssoBridgeTokenA, ssoBridgeCodeB: ssoBridgeTokenB},
		people:         people,
		authorizeCalls: map[string]int{},
		tokenCalls:     map[string]int{},
		userinfoCalls:  map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		query := r.URL.Query()
		code, known := provider.codes[host]
		provider.mu.Lock()
		provider.authorizeCalls[host]++
		expected := provider.redirectURI
		provider.mu.Unlock()
		redirect, state := query.Get("redirect_uri"), query.Get("state")
		if !known || query.Get("response_type") != "code" || query.Get("client_id") != provider.clients[code] ||
			state == "" || expected == "" || redirect != expected {
			provider.mu.Lock()
			provider.refused = append(provider.refused, r.URL.String())
			provider.mu.Unlock()
			http.Error(w, "unexpected authorization request", http.StatusBadRequest)
			return
		}
		target, err := url.Parse(redirect)
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		back := target.Query()
		back.Set("code", code)
		back.Set("state", state)
		target.RawQuery = back.Encode()
		// Back to the app by a script navigation, as a form-post or
		// interstitial provider page does.
		destination, _ := json.Marshal(target.String())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<!doctype html><title>Fake identity provider</title><script>location.replace(%s)</script>", destination)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ParseForm() != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		code := r.PostForm.Get("code")
		client, _, ok := r.BasicAuth()
		if !ok {
			client = r.PostForm.Get("client_id")
		}
		provider.mu.Lock()
		provider.tokenCalls[code]++
		provider.mu.Unlock()
		token, known := provider.tokens[code]
		if !known || provider.clients[code] != client {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": token,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		provider.mu.Lock()
		provider.userinfoCalls[token]++
		provider.mu.Unlock()
		person, known := provider.people[token]
		if !known {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(person)
	})
	provider.server = httptest.NewTLSServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

// expectRedirect sets the one redirect URI the authorization page accepts.
func (p *ssoBridgeProvider) expectRedirect(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redirectURI = uri
}

func (p *ssoBridgeProvider) calls() (authorize, tokens, userinfo map[string]int, refused []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copyCounts := func(from map[string]int) map[string]int {
		to := map[string]int{}
		for key, count := range from {
			to[key] = count
		}
		return to
	}
	return copyCounts(p.authorizeCalls), copyCounts(p.tokenCalls), copyCounts(p.userinfoCalls), append([]string(nil), p.refused...)
}

// ssoBridgeCalls counts, at the listener, the auth requests that reached the
// real server and the status each got.
type ssoBridgeCalls struct {
	mu        sync.Mutex
	init      int
	callbacks int
	exchanges []int
	refreshes int
	logins    int
	logouts   int
}

func (c *ssoBridgeCalls) observe(ctx *fasthttp.RequestCtx) {
	method, path := string(ctx.Method()), string(ctx.Path())
	status := ctx.Response.StatusCode()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case method == fasthttp.MethodGet && path == "/api/auth/sso/custom/init":
		c.init++
	case method == fasthttp.MethodGet && path == "/api/auth/sso/custom/callback":
		c.callbacks++
	case method == fasthttp.MethodPost && path == "/api/auth/refresh":
		if bytes.Contains(ctx.PostBody(), []byte(`"sso_exchange"`)) {
			c.exchanges = append(c.exchanges, status)
		} else {
			c.refreshes++
		}
	case method == fasthttp.MethodPost && path == "/api/auth/login":
		c.logins++
	case method == fasthttp.MethodPost && path == "/api/auth/logout":
		c.logouts++
	}
}

// ssoBridgeSummary is what the Playwright case reports back.
type ssoBridgeSummary struct {
	Callbacks      int      `json:"callbacks"`
	Exchanges      []string `json:"exchanges"`
	Logins         int      `json:"logins"`
	OtherRefreshes int      `json:"otherRefreshes"`
	AttemptA       string   `json:"attemptA"`
	AttemptB       string   `json:"attemptB"`
	MeEmail        string   `json:"meEmail"`
	MeOrganization string   `json:"meOrganization"`
}

func TestSSOBridgeRealBrowserTwoCallbacks(t *testing.T) {
	if os.Getenv(ssoBridgeOptInEnv) != "1" {
		t.Skip("real-browser SSO bridge (R1) is opt-in: set " + ssoBridgeOptInEnv +
			"=1 with TEST_DATABASE_URL, TEST_REDIS_URL, the frontend built into internal/frontend/dist, and Playwright")
	}
	for _, name := range []string{"TEST_DATABASE_URL", "TEST_REDIS_URL"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s=1 needs %s", ssoBridgeOptInEnv, name)
		}
	}
	frontendDir, err := filepath.Abs(filepath.Join("..", "..", "frontend"))
	require.NoError(t, err)
	playwrightCLI := filepath.Join(frontendDir, "node_modules", "@playwright", "test", "cli.js")
	if _, err := os.Stat(playwrightCLI); err != nil {
		t.Fatalf("Playwright is not installed (%v): run npm ci in frontend", err)
	}
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required for the Playwright case")

	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	require.NotNil(t, rdb, "TEST_REDIS_URL did not give a Redis client")

	// Two organizations, one SSO user each, each organization with its own
	// enabled custom provider.
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	userA := testutil.CreateTestUser(t, db, orgA.ID, testutil.WithEmail(testutil.UniqueEmail("sso-bridge-a")))
	userB := testutil.CreateTestUser(t, db, orgB.ID, testutil.WithEmail(testutil.UniqueEmail("sso-bridge-b")))
	provider := newSSOBridgeProvider(t, map[string]ssoBridgeIdentity{
		ssoBridgeTokenA: {Subject: "bridge-subject-a", Email: userA.Email, Name: "Bridge User A"},
		ssoBridgeTokenB: {Subject: "bridge-subject-b", Email: userB.Email, Name: "Bridge User B"},
	})
	secret, err := appcrypto.Encrypt("lane-e-bridge-client-secret", ssoBridgeEncryptionKey)
	require.NoError(t, err)
	for _, seed := range []struct {
		org    uuid.UUID
		client string
		origin string
	}{
		{orgA.ID, ssoBridgeClientA, ssoBridgeProviderA},
		{orgB.ID, ssoBridgeClientB, ssoBridgeProviderB},
	} {
		require.NoError(t, db.Create(&models.SSOProvider{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  seed.org,
			Provider:        "custom",
			ClientID:        seed.client,
			ClientSecret:    secret,
			IsEnabled:       true,
			AllowAutoCreate: false,
			DefaultRoleName: "agent",
			AuthURL:         seed.origin + "/auth",
			TokenURL:        seed.origin + "/token",
			UserInfoURL:     seed.origin + "/userinfo",
		}).Error)
	}

	// The real app: production middleware chain and routes. Its outbound
	// HTTP may reach only the two fake provider origins.
	cfg := &config.Config{
		App: config.AppConfig{Environment: "test", EncryptionKey: ssoBridgeEncryptionKey},
		JWT: config.JWTConfig{
			Secret:            testutil.TestJWTSecret,
			AccessExpiryMins:  15,
			RefreshExpiryDays: 7,
		},
		Cookie: config.CookieConfig{Secure: true},
	}
	app := &handlers.App{
		Config: cfg,
		DB:     db,
		Redis:  rdb,
		Log:    testutil.NopLogger(),
		HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
			ssoBridgeProviderA: provider.server,
			ssoBridgeProviderB: provider.server,
		}),
	}
	g := fastglue.NewGlue()
	g.Before(middleware.SecurityHeaders())
	g.Before(middleware.Recovery(app.Log))
	g.Before(middleware.CSRFProtection())
	setupRoutes(g, app, app.Log, "", rdb, cfg)
	routes := g.Handler()

	// The candidate frontend must be embedded in this binary.
	index := laneAInvoke(g, fasthttp.MethodGet, "/", nil, nil, nil)
	indexBody := string(index.Response.Body())
	require.Equal(t, fasthttp.StatusOK, index.Response.StatusCode(),
		"frontend not embedded: build frontend and copy frontend/dist into internal/frontend/dist before compiling this test")
	require.Contains(t, indexBody, `id="app"`,
		"frontend not embedded: build frontend and copy frontend/dist into internal/frontend/dist before compiling this test")
	bundle := regexp.MustCompile(`src="\.?(/assets/[^"]+\.js)"`).FindStringSubmatch(indexBody)
	require.Len(t, bundle, 2, "embedded index.html names no JS bundle")
	asset := laneAInvoke(g, fasthttp.MethodGet, bundle[1], nil, nil, nil)
	require.Equal(t, fasthttp.StatusOK, asset.Response.StatusCode(), "embedded JS bundle %s is missing", bundle[1])
	indexSum := sha256.Sum256(index.Response.Body())
	t.Logf("embedded frontend: index.html sha256 %s, bundle %s", hex.EncodeToString(indexSum[:]), bundle[1])

	// Serve it on a private loopback TLS listener, counting the auth calls
	// that reach it.
	calls := &ssoBridgeCalls{}
	certificate, key, err := fasthttp.GenerateTestCertificate("127.0.0.1")
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &fasthttp.Server{
		Name: "rereply-sso-bridge",
		Handler: func(ctx *fasthttp.RequestCtx) {
			routes(ctx)
			calls.observe(ctx)
		},
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeTLSEmbed(listener, certificate, key) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(shutdown)
		<-served
	})
	baseURL := "https://" + listener.Addr().String()
	provider.expectRedirect(baseURL + "/api/auth/sso/custom/callback")
	providerAddress := provider.server.Listener.Addr().String()
	hostRules := fmt.Sprintf("MAP %s %s, MAP %s %s",
		strings.TrimPrefix(ssoBridgeProviderA, "https://"), providerAddress,
		strings.TrimPrefix(ssoBridgeProviderB, "https://"), providerAddress)

	// The browser case, owned by this test: finite budget, cancelled and
	// joined here, its whole process tree killed on timeout.
	reportPath := filepath.Join(t.TempDir(), "sso-bridge-report.json")
	summaryPath := filepath.Join(t.TempDir(), "sso-bridge-summary.json")
	budget, cancel := context.WithTimeout(context.Background(), ssoBridgeBrowserBudget)
	defer cancel()
	cmd := exec.CommandContext(budget, node, playwrightCLI, "test",
		"--config", "playwright.sso-bridge.config.ts",
		"--grep", ssoBridgeTestTitle)
	cmd.Dir = frontendDir
	cmd.Env = append(os.Environ(),
		"BASE_URL="+baseURL,
		"SSO_BRIDGE_REPORT="+reportPath,
		"SSO_BRIDGE_SUMMARY="+summaryPath,
		"SSO_BRIDGE_SLUG_A="+orgA.Slug,
		"SSO_BRIDGE_SLUG_B="+orgB.Slug,
		"SSO_BRIDGE_ORG_A="+orgA.ID.String(),
		"SSO_BRIDGE_ORG_B="+orgB.ID.String(),
		"SSO_BRIDGE_EMAIL_A="+userA.Email,
		"SSO_BRIDGE_EMAIL_B="+userB.Email,
		"SSO_BRIDGE_IDP_A="+ssoBridgeProviderA,
		"SSO_BRIDGE_IDP_B="+ssoBridgeProviderB,
		"SSO_BRIDGE_CLIENT_A="+ssoBridgeClientA,
		"SSO_BRIDGE_CLIENT_B="+ssoBridgeClientB,
		"SSO_BRIDGE_CODE_A="+ssoBridgeCodeA,
		"SSO_BRIDGE_CODE_B="+ssoBridgeCodeB,
		"SSO_BRIDGE_HOST_RULES="+hostRules,
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 15 * time.Second
	runErr := cmd.Run()
	t.Logf("playwright output:\n%s", output.String())
	require.NoError(t, budget.Err(), "the browser case ran past its %s budget", ssoBridgeBrowserBudget)
	require.NoError(t, runErr, "the browser case failed")

	// Exactly one case ran and passed: an unexercised, skipped or retried
	// case is a failure here.
	var report struct {
		Stats struct {
			Expected   int `json:"expected"`
			Unexpected int `json:"unexpected"`
			Flaky      int `json:"flaky"`
			Skipped    int `json:"skipped"`
		} `json:"stats"`
	}
	reportBytes, err := os.ReadFile(reportPath)
	require.NoError(t, err, "Playwright wrote no JSON report")
	require.NoError(t, json.Unmarshal(reportBytes, &report))
	assert.Equal(t, 1, report.Stats.Expected, "exactly one passing browser case")
	assert.Zero(t, report.Stats.Unexpected)
	assert.Zero(t, report.Stats.Flaky)
	assert.Zero(t, report.Stats.Skipped, "a skipped browser case is a failure here")

	var summary ssoBridgeSummary
	summaryBytes, err := os.ReadFile(summaryPath)
	require.NoError(t, err, "the browser case wrote no summary")
	require.NoError(t, json.Unmarshal(summaryBytes, &summary))
	t.Logf("browser summary: %+v", summary)
	assert.Equal(t, 2, summary.Callbacks)
	assert.Equal(t, []string{"A", "B"}, summary.Exchanges)
	assert.Zero(t, summary.Logins)
	assert.Zero(t, summary.OtherRefreshes)
	assert.NotEqual(t, summary.AttemptA, summary.AttemptB)
	assert.Equal(t, userB.Email, summary.MeEmail)
	assert.Equal(t, orgB.ID.String(), summary.MeOrganization)

	// What reached the real server.
	calls.mu.Lock()
	reached := fmt.Sprintf("init=%d callbacks=%d exchanges=%v refreshes=%d logins=%d logouts=%d",
		calls.init, calls.callbacks, calls.exchanges, calls.refreshes, calls.logins, calls.logouts)
	assert.Equal(t, 2, calls.init, reached)
	assert.Equal(t, 2, calls.callbacks, reached)
	assert.Equal(t, []int{fasthttp.StatusOK, fasthttp.StatusOK}, calls.exchanges, reached)
	assert.Zero(t, calls.logins, reached)
	assert.Zero(t, calls.refreshes, reached)
	assert.Zero(t, calls.logouts, reached)
	calls.mu.Unlock()
	t.Logf("server-side auth calls: %s", reached)

	// One authorization per provider host, each code redeemed once by its
	// own provider's client, each token used once for userinfo, and no
	// exchange code left in Redis.
	authorizeCalls, tokenCalls, userinfoCalls, refused := provider.calls()
	assert.Empty(t, refused, "the fake provider refused an authorization request")
	assert.Equal(t, map[string]int{
		strings.TrimPrefix(ssoBridgeProviderA, "https://"): 1,
		strings.TrimPrefix(ssoBridgeProviderB, "https://"): 1,
	}, authorizeCalls)
	assert.Equal(t, map[string]int{ssoBridgeCodeA: 1, ssoBridgeCodeB: 1}, tokenCalls)
	assert.Equal(t, map[string]int{ssoBridgeTokenA: 1, ssoBridgeTokenB: 1}, userinfoCalls)
	leftover, err := rdb.Keys(context.Background(), "sso:exchange:*").Result()
	require.NoError(t, err)
	assert.Empty(t, leftover, "every exchange code was spent")
}
