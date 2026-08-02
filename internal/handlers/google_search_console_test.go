package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/fastglue"
)

func TestGoogleSearchConsoleWebsitePropertyType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		siteURL  string
		wantType string
		wantOK   bool
	}{
		{name: "domain", siteURL: "sc-domain:example.com", wantType: "domain", wantOK: true},
		{name: "punycode domain", siteURL: "sc-domain:xn--bcher-kva.example", wantType: "domain", wantOK: true},
		{name: "URL prefix", siteURL: "https://www.example.com/store/", wantType: "url_prefix", wantOK: true},
		{name: "HTTP URL prefix", siteURL: "http://example.test/", wantType: "url_prefix", wantOK: true},
		{name: "empty domain", siteURL: "sc-domain:", wantOK: false},
		{name: "empty label", siteURL: "sc-domain:bad..example", wantOK: false},
		{name: "leading hyphen", siteURL: "sc-domain:-bad.example", wantOK: false},
		{name: "credentials", siteURL: "https://user@example.test/", wantOK: false},
		{name: "fragment", siteURL: "https://example.test/#fragment", wantOK: false},
		{name: "unsupported scheme", siteURL: "ftp://example.test/", wantOK: false},
		{name: "Instagram platform property", siteURL: "https://www.instagram.com/example", wantOK: false},
		{name: "TikTok platform property", siteURL: "https://business.tiktok.com/example", wantOK: false},
		{name: "X platform property", siteURL: "https://x.com/example", wantOK: false},
		{name: "YouTube platform property", siteURL: "https://www.youtube.com/@example", wantOK: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotType, gotOK := googleSearchConsoleWebsitePropertyType(test.siteURL)
			assert.Equal(t, test.wantOK, gotOK)
			assert.Equal(t, test.wantType, gotType)
		})
	}
}

func TestGoogleSearchConsoleVerifiedPermission(t *testing.T) {
	t.Parallel()
	for _, permission := range []string{"siteOwner", "siteFullUser", "siteRestrictedUser"} {
		assert.True(t, googleSearchConsoleVerifiedPermission(permission), permission)
	}
	for _, permission := range []string{"", "siteUnverifiedUser", "owner", "SITEOWNER"} {
		assert.False(t, googleSearchConsoleVerifiedPermission(permission), permission)
	}
}

func TestGoogleSearchConsolePageBelongsToProperty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		page         string
		siteURL      string
		propertyType string
		want         bool
	}{
		{name: "domain apex", page: "https://example.com/page", siteURL: "sc-domain:example.com", propertyType: "domain", want: true},
		{name: "domain subdomain", page: "http://shop.example.com/page", siteURL: "sc-domain:example.com", propertyType: "domain", want: true},
		{name: "domain suffix attack", page: "https://notexample.com/page", siteURL: "sc-domain:example.com", propertyType: "domain", want: false},
		{name: "URL prefix child", page: "https://example.com/store/item?q=1", siteURL: "https://example.com/store/", propertyType: "url_prefix", want: true},
		{name: "URL prefix outside path", page: "https://example.com/stores/item", siteURL: "https://example.com/store/", propertyType: "url_prefix", want: false},
		{name: "URL prefix wrong scheme", page: "http://example.com/store/item", siteURL: "https://example.com/store/", propertyType: "url_prefix", want: false},
		{name: "URL prefix wrong port", page: "https://example.com:8443/store/item", siteURL: "https://example.com/store/", propertyType: "url_prefix", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, googleSearchConsolePageBelongsToProperty(test.page, test.siteURL, test.propertyType))
		})
	}
}

func TestParseGoogleSearchVisibilityParameters(t *testing.T) {
	t.Parallel()
	end := time.Now().UTC().AddDate(0, 0, -2)
	start := end.AddDate(0, 0, -6)
	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "property_id", uuid.NewString())
	testutil.SetQueryParam(req, "start_date", start.Format("2006-01-02"))
	testutil.SetQueryParam(req, "end_date", end.Format("2006-01-02"))
	testutil.SetQueryParam(req, "search_type", "googleNews")
	testutil.SetQueryParam(req, "page", "https://example.test/article")
	testutil.SetQueryParam(req, "limit", 25)

	parameters, err := parseGoogleSearchVisibilityParameters(req)
	require.NoError(t, err)
	assert.Equal(t, start.Format("2006-01-02"), parameters.StartDate.Format("2006-01-02"))
	assert.Equal(t, end.Format("2006-01-02"), parameters.EndDate.Format("2006-01-02"))
	assert.Equal(t, "googleNews", parameters.SearchType)
	assert.Equal(t, "https://example.test/article", parameters.Page)
	assert.Equal(t, 25, parameters.Limit)
}

func TestParseGoogleSearchVisibilityParametersRejectsUnsafeInput(t *testing.T) {
	t.Parallel()
	end := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	tooRecent := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	tooRecentStart := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")
	tests := []struct {
		name  string
		query map[string]string
	}{
		{name: "one date", query: map[string]string{"start_date": end}},
		{name: "final data lag", query: map[string]string{"start_date": tooRecentStart, "end_date": tooRecent}},
		{name: "bad search type", query: map[string]string{"search_type": "shopping"}},
		{name: "page credentials", query: map[string]string{"page": "https://user:secret@example.test/"}},
		{name: "page fragment", query: map[string]string{"page": "https://example.test/#fragment"}},
		{name: "bad property ID", query: map[string]string{"property_id": "not-a-uuid"}},
		{name: "zero limit", query: map[string]string{"limit": "0"}},
		{name: "large limit", query: map[string]string{"limit": "101"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := testutil.NewGETRequest(t)
			for key, value := range test.query {
				testutil.SetQueryParam(req, key, value)
			}
			_, err := parseGoogleSearchVisibilityParameters(req)
			require.Error(t, err)
		})
	}
}

func TestValidatedGoogleEndpointRequiresHTTPSExceptLoopback(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"https://oauth2.googleapis.com/token",
		"http://localhost:8080/token",
		"http://127.0.0.1:8080/token",
		"http://[::1]:8080/token",
	} {
		value, err := validatedGoogleEndpoint(endpoint)
		require.NoError(t, err, endpoint)
		assert.Equal(t, endpoint, value)
	}
	for _, endpoint := range []string{
		"http://oauth.example.test/token",
		"https://user:secret@oauth.example.test/token",
		"https://oauth.example.test/token#fragment",
		"javascript:alert(1)",
	} {
		_, err := validatedGoogleEndpoint(endpoint)
		require.Error(t, err, endpoint)
	}
}

func TestGoogleSearchConsoleCallbackRedirectSanitizesBasePath(t *testing.T) {
	app := &App{Config: &config.Config{Server: config.ServerConfig{BasePath: `/\evil.example`}}}
	request := testutil.NewGETRequest(t)
	request.RequestCtx.Request.SetRequestURI("https://crm.example.test/api/integrations/google_search_console/callback")
	request.RequestCtx.Request.Header.SetHost("crm.example.test")
	app.redirectGoogleSearchConsoleCallback(request, "connected")
	assert.Equal(t, http.StatusSeeOther, testutil.GetResponseStatusCode(request))
	location := string(request.RequestCtx.Response.Header.Peek("Location"))
	assert.NotContains(t, location, `\`)
	assert.NotRegexp(t, `^//`, location)
	assert.Contains(t, location, "/evil.example/settings/integrations?google_search_console=connected")
}

func TestGoogleSearchConsoleIntegrationResponsePreservesHealthAndDecryptability(t *testing.T) {
	encryptedToken, err := appcrypto.Encrypt("healthy-refresh-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	row := &models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		Provider:       integrationProviderGoogleSearchConsole,
		Enabled:        true,
		CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: encryptedToken},
	}
	app := &App{Config: &config.Config{
		App: config.AppConfig{EncryptionKey: integrationTestEncryptionKey},
		GoogleSearchConsole: config.GoogleSearchConsoleConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.test/api/integrations/google_search_console/callback",
			AuthURL:      "https://accounts.google.com/o/oauth2/auth",
			TokenURL:     "https://oauth2.googleapis.com/token",
			APIBaseURL:   "https://www.googleapis.com/webmasters/v3",
		},
	}}
	sources := &integrationSources{
		rows: map[string]*models.ProviderIntegration{integrationProviderGoogleSearchConsole: row},
		connections: map[string]integrationConnectionResponse{
			integrationProviderGoogleSearchConsole: {AccountCount: 1, ActiveCount: 1},
		},
	}

	healthy := app.composeIntegrationResponse(integrationProviderGoogleSearchConsole, sources)
	assert.Equal(t, integrationStatusConnected, healthy.Status)
	assert.True(t, healthy.credentialUsable)

	row.LastErrorCode = integrationValidationFailedCode
	row.LastErrorMessage = "Provider validation failed"
	degraded := app.composeIntegrationResponse(integrationProviderGoogleSearchConsole, sources)
	assert.Equal(t, integrationStatusDegraded, degraded.Status)
	assert.Equal(t, "Provider validation failed", degraded.Connection.LastError)

	row.LastErrorCode = integrationAnalyticsFailedCode
	row.LastErrorMessage = "Provider analytics request failed"
	transientAnalyticsFailure := app.composeIntegrationResponse(integrationProviderGoogleSearchConsole, sources)
	assert.Equal(t, integrationStatusConnected, transientAnalyticsFailure.Status)
	assert.Equal(t, "Provider analytics request failed", transientAnalyticsFailure.Connection.LastError)

	row.LastErrorCode = ""
	row.LastErrorMessage = ""
	app.Config.App.EncryptionKey = "different-encryption-key"
	undecryptable := app.composeIntegrationResponse(integrationProviderGoogleSearchConsole, sources)
	assert.Equal(t, integrationStatusDegraded, undecryptable.Status)
	assert.False(t, undecryptable.credentialUsable)
	assert.Contains(t, undecryptable.Message, "stored Google authorization is unavailable")
}

func TestGoogleSearchVisibilityQueriesOpaquePropertyAndExactPage(t *testing.T) {
	t.Parallel()
	var mutex sync.Mutex
	requestCount := 0
	dimensionsSeen := map[string]bool{}
	page := "https://example.test/store/item?a=1"
	property := models.GoogleSearchConsoleProperty{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		SiteURL:      "https://example.test/",
		DisplayName:  "example.test",
		PropertyType: "url_prefix",
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		wantPath := "/sites/" + url.PathEscape(property.SiteURL) + "/searchAnalytics/query"
		assert.Equal(t, wantPath, request.URL.EscapedPath())
		var query googleSearchAnalyticsRequest
		require.NoError(t, json.NewDecoder(request.Body).Decode(&query))
		require.Equal(t, "final", query.DataState)
		require.Equal(t, "web", query.Type)
		require.Len(t, query.DimensionFilterGroups, 1)
		require.Len(t, query.DimensionFilterGroups[0].Filters, 1)
		assert.Equal(t, googleSearchDimensionFilter{Dimension: "page", Operator: "equals", Expression: page}, query.DimensionFilterGroups[0].Filters[0])

		dimension := strings.Join(query.Dimensions, ",")
		mutex.Lock()
		requestCount++
		dimensionsSeen[dimension] = true
		mutex.Unlock()

		response.Header().Set("Content-Type", "application/json")
		switch dimension {
		case "":
			_, _ = response.Write([]byte(`{"rows":[{"clicks":12,"impressions":120,"ctr":0.1,"position":3.4}]}`))
		case "date":
			_, _ = response.Write([]byte(`{"rows":[{"keys":["2026-07-01"],"clicks":5,"impressions":50,"ctr":0.1,"position":3.2}]}`))
		case "query":
			_, _ = response.Write([]byte(`{"rows":[{"keys":["clinic near me"],"clicks":7,"impressions":70,"ctr":0.1,"position":2.8}]}`))
		case "page":
			_, _ = response.Write([]byte(`{"rows":[{"keys":["https://example.test/store/item?a=1"],"clicks":12,"impressions":120,"ctr":0.1,"position":3.4}]}`))
		default:
			http.Error(response, "unexpected dimensions", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	app := &App{Config: &config.Config{GoogleSearchConsole: config.GoogleSearchConsoleConfig{APIBaseURL: server.URL}}}
	parameters := googleSearchVisibilityParameters{
		StartDate:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
		SearchType: "web",
		Page:       page,
		Limit:      10,
	}
	result, err := app.fetchGoogleSearchVisibility(context.Background(), server.Client(), property, parameters)
	require.NoError(t, err)
	assert.Equal(t, float64(12), result.Summary.Clicks)
	assert.Len(t, result.Trend, 2)
	assert.Equal(t, float64(5), result.Trend[0].Clicks)
	assert.Equal(t, float64(0), result.Trend[1].Clicks)
	require.Len(t, result.TopQueries, 1)
	assert.Equal(t, "clinic near me", result.TopQueries[0].Query)
	require.Len(t, result.TopPages, 1)
	assert.Equal(t, page, result.TopPages[0].Page)
	assert.Equal(t, "final", result.DataState)

	mutex.Lock()
	defer mutex.Unlock()
	assert.Equal(t, 4, requestCount)
	for _, dimension := range []string{"", "date", "query", "page"} {
		assert.True(t, dimensionsSeen[dimension], dimension)
	}
}

func TestGoogleSearchConsoleOAuthConnectCallbackReplayAndAccountSafeReconnect(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("Redis is required for the OAuth state test")
	}
	app.Redis = redisClient
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)

	var mutex sync.Mutex
	tokenRequests := 0
	siteRequests := 0
	verifiers := make([]string, 0, 3)
	fakeGoogle := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			require.NoError(t, request.ParseForm())
			mutex.Lock()
			tokenRequests++
			requestNumber := tokenRequests
			verifiers = append(verifiers, request.Form.Get("code_verifier"))
			mutex.Unlock()
			response.Header().Set("Content-Type", "application/json")
			if requestNumber == 1 {
				_, _ = response.Write([]byte(`{"access_token":"access-one","refresh_token":"refresh-one","token_type":"Bearer","expires_in":3600}`))
				return
			}
			if requestNumber == 2 {
				// A missing refresh token must fail closed: the access token could
				// belong to a different Google account than the stored token.
				_, _ = response.Write([]byte(`{"access_token":"access-two","token_type":"Bearer","expires_in":3600}`))
				return
			}
			_, _ = response.Write([]byte(`{"access_token":"access-three","refresh_token":"refresh-two","token_type":"Bearer","expires_in":3600}`))
		case "/sites":
			mutex.Lock()
			siteRequests++
			mutex.Unlock()
			assert.Contains(t, request.Header.Get("Authorization"), "Bearer access-")
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"siteEntry":[{"siteUrl":"sc-domain:example.test","permissionLevel":"siteOwner"},{"siteUrl":"https://www.instagram.com/example","permissionLevel":"siteOwner"},{"siteUrl":"https://unverified.example.test/","permissionLevel":"siteUnverifiedUser"}]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer fakeGoogle.Close()
	app.HTTPClient = fakeGoogle.Client()
	app.Config.GoogleSearchConsole = config.GoogleSearchConsoleConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://localhost/api/integrations/google_search_console/callback",
		AuthURL:      fakeGoogle.URL + "/auth",
		TokenURL:     fakeGoogle.URL + "/token",
		APIBaseURL:   fakeGoogle.URL,
	}

	type connectResponse struct {
		Ready            bool   `json:"ready"`
		AuthorizationURL string `json:"authorization_url"`
	}
	startConnect := func() (connectResponse, googleSearchConsoleOAuthState) {
		t.Helper()
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, admin.ID)
		testutil.SetPathParam(req, "provider", integrationProviderGoogleSearchConsole)
		require.NoError(t, app.ConnectIntegration(req))
		require.Equal(t, http.StatusOK, testutil.GetResponseStatusCode(req))
		var result connectResponse
		testutil.ParseEnvelopeResponse(t, req, &result)
		require.True(t, result.Ready)
		parsedAuthURL, err := url.Parse(result.AuthorizationURL)
		require.NoError(t, err)
		stateNonce := parsedAuthURL.Query().Get("state")
		require.NotEmpty(t, stateNonce)
		stateJSON, err := app.Redis.Get(context.Background(), googleSearchConsoleOAuthStatePrefix+stateNonce).Bytes()
		require.NoError(t, err)
		var state googleSearchConsoleOAuthState
		require.NoError(t, json.Unmarshal(stateJSON, &state))
		assert.Equal(t, org.ID.String(), state.OrganizationID)
		assert.Equal(t, admin.ID.String(), state.UserID)
		assert.Equal(t, stateNonce, state.Nonce)
		assert.Len(t, state.CodeVerifier, 43)
		assert.Equal(t, googleSearchConsoleReadOnlyScope, parsedAuthURL.Query().Get("scope"))
		assert.Equal(t, "offline", parsedAuthURL.Query().Get("access_type"))
		assert.Equal(t, "S256", parsedAuthURL.Query().Get("code_challenge_method"))
		assert.NotEmpty(t, parsedAuthURL.Query().Get("code_challenge"))
		assert.Empty(t, parsedAuthURL.Query().Get("include_granted_scopes"))
		assert.Equal(t, "consent select_account", parsedAuthURL.Query().Get("prompt"))
		return result, state
	}
	completeCallback := func(state googleSearchConsoleOAuthState, code string) *fastglue.Request {
		t.Helper()
		req := testutil.NewGETRequest(t)
		testutil.SetQueryParam(req, "state", state.Nonce)
		testutil.SetQueryParam(req, "code", code)
		require.NoError(t, app.CallbackGoogleSearchConsole(req))
		return req
	}

	firstConnect, firstState := startConnect()
	parsedFirstURL, err := url.Parse(firstConnect.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "consent select_account", parsedFirstURL.Query().Get("prompt"))
	firstCallback := completeCallback(firstState, "first-code")
	assert.Equal(t, http.StatusSeeOther, testutil.GetResponseStatusCode(firstCallback))
	assert.Equal(t, "/settings/integrations?google_search_console=connected", string(firstCallback.RequestCtx.Response.Header.Peek("Location")))

	var row models.ProviderIntegration
	require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, integrationProviderGoogleSearchConsole).First(&row).Error)
	storedRefreshToken, _ := row.CredentialData[googleSearchConsoleRefreshTokenCredential].(string)
	assert.True(t, appcrypto.IsEncrypted(storedRefreshToken))
	assert.NotContains(t, storedRefreshToken, "refresh-one")
	decryptedRefreshToken, err := appcrypto.Decrypt(storedRefreshToken, integrationTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "refresh-one", decryptedRefreshToken)

	var properties []models.GoogleSearchConsoleProperty
	require.NoError(t, app.DB.Where("organization_id = ?", org.ID).Find(&properties).Error)
	require.Len(t, properties, 1)
	assert.Equal(t, "sc-domain:example.test", properties[0].SiteURL)
	assert.Equal(t, "domain", properties[0].PropertyType)
	assert.False(t, properties[0].Selected)

	// OAuth state is consumed atomically: replay never reaches the token server.
	replay := completeCallback(firstState, "replayed-code")
	assert.Equal(t, http.StatusSeeOther, testutil.GetResponseStatusCode(replay))
	assert.Equal(t, "/settings/integrations?google_search_console=error", string(replay.RequestCtx.Response.Header.Peek("Location")))
	mutex.Lock()
	assert.Equal(t, 1, tokenRequests)
	assert.Equal(t, 1, siteRequests)
	mutex.Unlock()

	secondConnect, secondState := startConnect()
	parsedSecondURL, err := url.Parse(secondConnect.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "consent select_account", parsedSecondURL.Query().Get("prompt"))
	secondCallback := completeCallback(secondState, "second-code")
	assert.Equal(t, http.StatusSeeOther, testutil.GetResponseStatusCode(secondCallback))
	assert.Equal(t, "/settings/integrations?google_search_console=error", string(secondCallback.RequestCtx.Response.Header.Peek("Location")))

	require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, integrationProviderGoogleSearchConsole).First(&row).Error)
	storedRefreshToken, _ = row.CredentialData[googleSearchConsoleRefreshTokenCredential].(string)
	decryptedRefreshToken, err = appcrypto.Decrypt(storedRefreshToken, integrationTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "refresh-one", decryptedRefreshToken)
	mutex.Lock()
	assert.Equal(t, 2, tokenRequests)
	assert.Equal(t, 1, siteRequests)
	require.Len(t, verifiers, 2)
	assert.Equal(t, firstState.CodeVerifier, verifiers[0])
	assert.Equal(t, secondState.CodeVerifier, verifiers[1])
	mutex.Unlock()

	_, thirdState := startConnect()
	thirdCallback := completeCallback(thirdState, "third-code")
	assert.Equal(t, http.StatusSeeOther, testutil.GetResponseStatusCode(thirdCallback))
	assert.Equal(t, "/settings/integrations?google_search_console=connected", string(thirdCallback.RequestCtx.Response.Header.Peek("Location")))
	require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, integrationProviderGoogleSearchConsole).First(&row).Error)
	storedRefreshToken, _ = row.CredentialData[googleSearchConsoleRefreshTokenCredential].(string)
	decryptedRefreshToken, err = appcrypto.Decrypt(storedRefreshToken, integrationTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "refresh-two", decryptedRefreshToken)
	mutex.Lock()
	assert.Equal(t, 3, tokenRequests)
	assert.Equal(t, 2, siteRequests)
	require.Len(t, verifiers, 3)
	assert.Equal(t, thirdState.CodeVerifier, verifiers[2])
	mutex.Unlock()
}

func TestGoogleSearchConsoleRejectsCrossTenantPropertyIDs(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.GoogleSearchConsole = config.GoogleSearchConsoleConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://app.example.test/api/integrations/google_search_console/callback",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		APIBaseURL:   "https://www.googleapis.com/webmasters/v3",
	}
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	userA := integrationTestUser(
		t,
		app,
		orgA.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
		models.ResourceAnalytics+":"+models.ActionRead,
	)
	encryptedRefreshToken, err := appcrypto.Encrypt("tenant-a-refresh", integrationTestEncryptionKey)
	require.NoError(t, err)
	integrationA := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		Provider:       integrationProviderGoogleSearchConsole,
		Enabled:        true,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: encryptedRefreshToken},
	}
	integrationB := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		Provider:       integrationProviderGoogleSearchConsole,
		Enabled:        true,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: encryptedRefreshToken},
	}
	require.NoError(t, app.DB.Create(&integrationA).Error)
	require.NoError(t, app.DB.Create(&integrationB).Error)
	propertyB := models.GoogleSearchConsoleProperty{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgB.ID,
		IntegrationID:   integrationB.ID,
		SiteURL:         "sc-domain:tenant-b.example",
		SiteURLHash:     strings.Repeat("b", 64),
		DisplayName:     "tenant-b.example",
		PropertyType:    "domain",
		PermissionLevel: "siteOwner",
		Available:       true,
		Selected:        true,
	}
	require.NoError(t, app.DB.Create(&propertyB).Error)

	selectReq := testutil.NewJSONRequest(t, map[string]any{"property_ids": []string{propertyB.ID.String()}})
	testutil.SetAuthContext(selectReq, orgA.ID, userA.ID)
	require.NoError(t, app.UpdateGoogleSearchConsoleProperties(selectReq))
	assert.Equal(t, http.StatusBadRequest, testutil.GetResponseStatusCode(selectReq))
	assert.NotContains(t, string(testutil.GetResponseBody(selectReq)), propertyB.SiteURL)

	analyticsReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(analyticsReq, orgA.ID, userA.ID)
	testutil.SetQueryParam(analyticsReq, "property_id", propertyB.ID.String())
	testutil.SetQueryParam(analyticsReq, "start_date", time.Now().UTC().AddDate(0, 0, -8).Format("2006-01-02"))
	testutil.SetQueryParam(analyticsReq, "end_date", time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02"))
	require.NoError(t, app.GetGoogleSearchVisibility(analyticsReq))
	assert.Equal(t, http.StatusNotFound, testutil.GetResponseStatusCode(analyticsReq))
	assert.NotContains(t, string(testutil.GetResponseBody(analyticsReq)), propertyB.SiteURL)

	var reloadedPropertyB models.GoogleSearchConsoleProperty
	require.NoError(t, app.DB.First(&reloadedPropertyB, "id = ?", propertyB.ID).Error)
	assert.True(t, reloadedPropertyB.Selected)
}

func TestGoogleSearchConsoleDisconnectIsTenantLocal(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	var mutex sync.Mutex
	providerCalls := 0
	fakeGoogle := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		providerCalls++
		mutex.Unlock()
		response.WriteHeader(http.StatusNoContent)
	}))
	defer fakeGoogle.Close()
	app.HTTPClient = fakeGoogle.Client()
	app.Config.GoogleSearchConsole = config.GoogleSearchConsoleConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://localhost/api/integrations/google_search_console/callback",
		AuthURL:      fakeGoogle.URL + "/auth",
		TokenURL:     fakeGoogle.URL + "/token",
		APIBaseURL:   fakeGoogle.URL,
	}
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	adminA := integrationTestUser(
		t,
		app,
		orgA.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	sharedToken, err := appcrypto.Encrypt("shared-google-account-refresh", integrationTestEncryptionKey)
	require.NoError(t, err)
	rows := []models.ProviderIntegration{
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgA.ID,
			Provider:       integrationProviderGoogleSearchConsole,
			Enabled:        true,
			CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: sharedToken},
		},
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgB.ID,
			Provider:       integrationProviderGoogleSearchConsole,
			Enabled:        true,
			CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: sharedToken},
		},
	}
	require.NoError(t, app.DB.Create(&rows).Error)
	properties := []models.GoogleSearchConsoleProperty{
		{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgA.ID,
			IntegrationID:   rows[0].ID,
			SiteURL:         "sc-domain:tenant-a.example",
			SiteURLHash:     strings.Repeat("a", 64),
			DisplayName:     "tenant-a.example",
			PropertyType:    "domain",
			PermissionLevel: "siteOwner",
			Available:       true,
			Selected:        true,
		},
		{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgB.ID,
			IntegrationID:   rows[1].ID,
			SiteURL:         "sc-domain:tenant-b.example",
			SiteURLHash:     strings.Repeat("b", 64),
			DisplayName:     "tenant-b.example",
			PropertyType:    "domain",
			PermissionLevel: "siteOwner",
			Available:       true,
			Selected:        true,
		},
	}
	require.NoError(t, app.DB.Create(&properties).Error)

	disconnectReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(disconnectReq, orgA.ID, adminA.ID)
	require.NoError(t, app.DisconnectGoogleSearchConsole(disconnectReq))
	assert.Equal(t, http.StatusOK, testutil.GetResponseStatusCode(disconnectReq))
	mutex.Lock()
	assert.Zero(t, providerCalls, "tenant disconnect must not revoke a Google account-level grant")
	mutex.Unlock()

	var reloadedA, reloadedB models.ProviderIntegration
	require.NoError(t, app.DB.First(&reloadedA, "id = ?", rows[0].ID).Error)
	require.NoError(t, app.DB.First(&reloadedB, "id = ?", rows[1].ID).Error)
	_, tokenAExists := reloadedA.CredentialData[googleSearchConsoleRefreshTokenCredential]
	tokenB, tokenBExists := reloadedB.CredentialData[googleSearchConsoleRefreshTokenCredential]
	assert.False(t, tokenAExists)
	assert.True(t, tokenBExists)
	assert.Equal(t, sharedToken, tokenB)
	var reloadedPropertyA, reloadedPropertyB models.GoogleSearchConsoleProperty
	require.NoError(t, app.DB.First(&reloadedPropertyA, "id = ?", properties[0].ID).Error)
	require.NoError(t, app.DB.First(&reloadedPropertyB, "id = ?", properties[1].ID).Error)
	assert.False(t, reloadedPropertyA.Available)
	assert.False(t, reloadedPropertyA.Selected)
	assert.True(t, reloadedPropertyB.Available)
	assert.True(t, reloadedPropertyB.Selected)
}

func TestGoogleSearchVisibilitySetupUsesAnalyticsPermission(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.GoogleSearchConsole = config.GoogleSearchConsoleConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://app.example.test/api/integrations/google_search_console/callback",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		APIBaseURL:   "https://www.googleapis.com/webmasters/v3",
	}
	org := testutil.CreateTestOrganization(t, app.DB)
	analyst := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceAnalytics+":"+models.ActionRead,
	)
	settingsReader := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
	)
	encryptedRefreshToken, err := appcrypto.Encrypt("analytics-setup-refresh", integrationTestEncryptionKey)
	require.NoError(t, err)
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Provider:       integrationProviderGoogleSearchConsole,
		Enabled:        true,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{googleSearchConsoleRefreshTokenCredential: encryptedRefreshToken},
	}
	require.NoError(t, app.DB.Create(&integration).Error)
	property := models.GoogleSearchConsoleProperty{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		IntegrationID:   integration.ID,
		SiteURL:         "sc-domain:analytics.example",
		SiteURLHash:     strings.Repeat("a", 64),
		DisplayName:     "analytics.example",
		PropertyType:    "domain",
		PermissionLevel: "siteOwner",
		Available:       true,
		Selected:        true,
	}
	require.NoError(t, app.DB.Create(&property).Error)

	setupReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(setupReq, org.ID, analyst.ID)
	require.NoError(t, app.GetGoogleSearchVisibilitySetup(setupReq))
	require.Equal(t, http.StatusOK, testutil.GetResponseStatusCode(setupReq))
	var setup googleSearchVisibilitySetupResponse
	testutil.ParseEnvelopeResponse(t, setupReq, &setup)
	assert.Equal(t, integrationStatusConnected, setup.Status)
	require.Len(t, setup.Properties, 1)
	assert.Equal(t, property.ID, setup.Properties[0].ID)
	assert.NotContains(t, string(testutil.GetResponseBody(setupReq)), "analytics-setup-refresh")
	assert.NotContains(t, string(testutil.GetResponseBody(setupReq)), "client-secret")

	require.NoError(t, app.DB.Model(&integration).Updates(map[string]any{
		"last_error_code":    integrationValidationFailedCode,
		"last_error_message": "Provider validation failed",
	}).Error)
	degradedReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(degradedReq, org.ID, analyst.ID)
	require.NoError(t, app.GetGoogleSearchVisibilitySetup(degradedReq))
	require.Equal(t, http.StatusOK, testutil.GetResponseStatusCode(degradedReq))
	var degraded googleSearchVisibilitySetupResponse
	testutil.ParseEnvelopeResponse(t, degradedReq, &degraded)
	assert.Equal(t, integrationStatusDegraded, degraded.Status)
	assert.Equal(t, "Provider validation failed", degraded.LastError)

	deniedReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(deniedReq, org.ID, settingsReader.ID)
	require.NoError(t, app.GetGoogleSearchVisibilitySetup(deniedReq))
	assert.Equal(t, http.StatusForbidden, testutil.GetResponseStatusCode(deniedReq))
}
