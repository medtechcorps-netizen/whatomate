package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const (
	metaLifecycleTestAppID         = "100000000000001"
	metaLifecycleTestConfigID      = "300000000000001"
	metaLifecycleTestAppSecret     = "server-only-meta-lifecycle-test-secret-at-least-32-bytes"
	metaLifecycleTestBusinessID    = "200000000000001"
	metaLifecycleTestPageID        = "700000000000001"
	metaLifecycleTestUserID        = "900000000000001"
	metaLifecycleTestEncryptionKey = "meta-lifecycle-test-encryption-key-long-enough"
)

type metaMessengerRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn metaMessengerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

var metaMessengerLifecycleEndpointHandlers = []struct {
	name    string
	handler func(*App, *fastglue.Request) error
}{
	{"status", (*App).GetMetaMessengerOnboardingStatus},
	{"start", (*App).StartMetaMessengerOnboarding},
	{"callback", (*App).ExchangeMetaMessengerOnboarding},
	{"select", (*App).SelectMetaMessengerOnboarding},
	{"reconnect", (*App).ReconnectMetaMessengerOnboarding},
	{"reconcile", (*App).ReconcileMetaMessengerSubscription},
	{"approve", (*App).ApproveMetaMessengerActivation},
	{"disconnect", (*App).DisconnectMetaMessenger},
}

func newMetaLifecycleGraphApp(t *testing.T, server *httptest.Server) *App {
	t.Helper()
	return &App{
		Config: &configpkg.Config{
			App:          configpkg.AppConfig{Environment: "test", EncryptionKey: metaLifecycleTestEncryptionKey},
			MetaRegistry: configpkg.MetaRegistryConfig{Enabled: true},
			MetaMessenger: configpkg.MetaMessengerConfig{
				Enabled: true, AppID: metaLifecycleTestAppID, ConfigID: metaLifecycleTestConfigID,
				AppSecret: metaLifecycleTestAppSecret, GraphAPIVersion: "v25.0",
				GraphBaseURL: "https://graph.meta.test", ReReplyBaseURL: "https://app.example.test",
				RelayBaseURL: "https://relay.example.test", AllowDevelopmentUserToken: true,
			},
		},
		HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
			"https://graph.meta.test": server,
		}),
	}
}

func newMetaLifecycleAuthorizationFixture(
	t *testing.T,
) (*App, *models.Organization, *models.Organization, *models.User) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	app := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &configpkg.Config{App: configpkg.AppConfig{Environment: "test"}},
	}
	home := testutil.CreateTestOrganization(t, db)
	target := testutil.CreateTestOrganization(t, db)
	homeRole := testutil.CreateTestRoleWithKeys(
		t, db, home.ID, "meta-messenger-home", []string{
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	user := testutil.CreateTestUser(t, db, home.ID, testutil.WithRoleID(&homeRole.ID))
	targetRole := testutil.CreateTestRoleWithKeys(
		t, db, target.ID, "meta-messenger-target", []string{
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	require.NoError(t, db.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID,
		OrganizationID: target.ID, RoleID: &targetRole.ID,
	}).Error)
	enableBookingCommerceTestEntitlement(t, db, home.ID, user.ID, "omnichannel.enabled")
	enableBookingCommerceTestEntitlement(t, db, target.ID, user.ID, "omnichannel.enabled")
	app.Config.MetaMessenger.AllowedOrganizationIDs = home.ID.String() + "," + target.ID.String()
	return app, home, target, user
}

func TestMetaMessengerLifecycleEndpointsRejectDefaultOrganizationFallback(t *testing.T) {
	app, home, _, user := newMetaLifecycleAuthorizationFixture(t)
	for _, endpoint := range metaMessengerLifecycleEndpointHandlers {
		for _, header := range []struct{ name, value string }{
			{name: "missing"},
			{name: "malformed", value: "not-a-workspace"},
		} {
			t.Run(endpoint.name+"_"+header.name, func(t *testing.T) {
				request := testutil.NewJSONRequest(t, map[string]any{})
				testutil.SetAuthContext(request, home.ID, user.ID)
				if header.value != "" {
					testutil.SetHeader(request, "X-Organization-ID", header.value)
				}
				require.NoError(t, endpoint.handler(app, request))
				testutil.AssertErrorResponse(
					t, request, fasthttp.StatusBadRequest,
					"X-Organization-ID must identify the selected organization",
				)
			})
		}
	}
}

func TestMetaMessengerLifecycleEndpointsRejectUnauthorizedAndDeletedOrganizations(t *testing.T) {
	app, home, target, user := newMetaLifecycleAuthorizationFixture(t)
	unauthorized := testutil.CreateTestOrganization(t, app.DB)
	require.NoError(t, app.DB.Delete(target).Error)

	for _, endpoint := range metaMessengerLifecycleEndpointHandlers {
		for _, targetOrganization := range []struct {
			name string
			id   uuid.UUID
		}{
			{name: "unauthorized", id: unauthorized.ID},
			{name: "deleted", id: target.ID},
		} {
			t.Run(endpoint.name+"_"+targetOrganization.name, func(t *testing.T) {
				request := testutil.NewJSONRequest(t, map[string]any{})
				testutil.SetAuthContext(request, home.ID, user.ID)
				testutil.SetHeader(request, "X-Organization-ID", targetOrganization.id.String())
				require.NoError(t, endpoint.handler(app, request))
				testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "not available")
			})
		}
	}
}

func TestMetaMessengerMutationRequiresIntegrationWriteBeforeRedisOrMeta(t *testing.T) {
	app, home, _, _ := newMetaLifecycleAuthorizationFixture(t)
	role := testutil.CreateTestRoleWithKeys(
		t,
		app.DB,
		home.ID,
		"meta-messenger-channel-only",
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceChannelAccounts + ":" + models.ActionDelete,
		},
	)
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithRoleID(&role.ID))
	app.Redis = nil
	var providerCalls atomic.Int32
	app.HTTPClient = &http.Client{Transport: metaMessengerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("unexpected Meta request")
	})}

	for _, endpoint := range metaMessengerLifecycleEndpointHandlers {
		if endpoint.name == "status" {
			continue
		}
		t.Run(endpoint.name, func(t *testing.T) {
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, home.ID, user.ID)
			testutil.SetHeader(request, "X-Organization-ID", home.ID.String())
			require.NoError(t, endpoint.handler(app, request))
			testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "Insufficient permissions")
		})
	}
	assert.Zero(t, providerCalls.Load())
}

func TestMetaMessengerReconciliationRequiresBothWriteAndDeleteBeforeProvider(t *testing.T) {
	app, home, _, _ := newMetaLifecycleAuthorizationFixture(t)
	var providerCalls atomic.Int32
	app.Redis = nil
	app.HTTPClient = &http.Client{Transport: metaMessengerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("unexpected Meta request")
	})}
	for _, testCase := range []struct {
		name        string
		permissions []string
	}{
		{
			name: "delete_only_fenced_subscribe",
			permissions: []string{
				models.ResourceChannelAccounts + ":" + models.ActionDelete,
				models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
			},
		},
		{
			name: "write_only_fenced_unsubscribe",
			permissions: []string{
				models.ResourceChannelAccounts + ":" + models.ActionWrite,
				models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			role := testutil.CreateTestRoleWithKeys(t, app.DB, home.ID, testCase.name, testCase.permissions)
			user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithRoleID(&role.ID))
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, home.ID, user.ID)
			testutil.SetHeader(request, "X-Organization-ID", home.ID.String())
			require.NoError(t, app.ReconcileMetaMessengerSubscription(request))
			testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "Insufficient permissions")
		})
	}
	assert.Zero(t, providerCalls.Load())
}

func TestMetaMessengerStatusPinsSwitchedOrganizationAndRejectsUnavailableTargets(t *testing.T) {
	app, home, target, user := newMetaLifecycleAuthorizationFixture(t)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, home.ID, user.ID)
	testutil.SetHeader(request, "X-Organization-ID", target.ID.String())
	require.NoError(t, app.GetMetaMessengerOnboardingStatus(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var response struct {
		Data struct {
			OrganizationID uuid.UUID `json:"organization_id"`
			Enabled        bool      `json:"enabled"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	assert.Equal(t, target.ID, response.Data.OrganizationID)
	assert.False(t, response.Data.Enabled)

	nonPilot := testutil.CreateTestOrganization(t, app.DB)
	nonPilotRole := testutil.CreateTestRoleWithKeys(
		t, app.DB, nonPilot.ID, "meta-messenger-non-pilot",
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	require.NoError(t, app.DB.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID,
		OrganizationID: nonPilot.ID, RoleID: &nonPilotRole.ID,
	}).Error)
	enableBookingCommerceTestEntitlement(t, app.DB, nonPilot.ID, user.ID, "omnichannel.enabled")
	for _, endpoint := range []struct {
		name    string
		handler func(*App, *fastglue.Request) error
	}{
		{name: "status", handler: (*App).GetMetaMessengerOnboardingStatus},
		{name: "start", handler: (*App).StartMetaMessengerOnboarding},
	} {
		t.Run("non_pilot_"+endpoint.name, func(t *testing.T) {
			nonPilotRequest := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(nonPilotRequest, home.ID, user.ID)
			testutil.SetHeader(nonPilotRequest, "X-Organization-ID", nonPilot.ID.String())
			require.NoError(t, endpoint.handler(app, nonPilotRequest))
			testutil.AssertErrorResponse(t, nonPilotRequest, fasthttp.StatusNotFound, "not available for this workspace")
		})
	}

	unauthorized := testutil.CreateTestOrganization(t, app.DB)
	request = testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, home.ID, user.ID)
	testutil.SetHeader(request, "X-Organization-ID", unauthorized.ID.String())
	require.NoError(t, app.GetMetaMessengerOnboardingStatus(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "not available")

	require.NoError(t, app.DB.Delete(target).Error)
	request = testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, home.ID, user.ID)
	testutil.SetHeader(request, "X-Organization-ID", target.ID.String())
	require.NoError(t, app.GetMetaMessengerOnboardingStatus(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "not available")
}

func TestMetaMessengerStatusAllowsSuperAdminToPinExactActiveOrganization(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &configpkg.Config{App: configpkg.AppConfig{Environment: "test"}},
	}
	home := testutil.CreateTestOrganization(t, db)
	target := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, home.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, target.ID, user.ID, "omnichannel.enabled")
	app.Config.MetaMessenger.AllowedOrganizationIDs = target.ID.String()

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, home.ID, user.ID)
	testutil.SetHeader(request, "X-Organization-ID", target.ID.String())
	require.NoError(t, app.GetMetaMessengerOnboardingStatus(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var response struct {
		Data struct {
			OrganizationID uuid.UUID `json:"organization_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	assert.Equal(t, target.ID, response.Data.OrganizationID)
}

func TestMetaMessengerCodeExchangeKeepsFLfBCodeContractServerSide(t *testing.T) {
	var captured url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/v25.0/oauth/access_token", request.URL.Path)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Empty(t, request.URL.RawQuery)
		require.NoError(t, request.ParseForm())
		captured = request.Form
		assert.NotContains(t, request.URL.String(), "authorization-code")
		assert.NotContains(t, request.URL.String(), metaLifecycleTestAppSecret)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"user-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer server.Close()
	app := newMetaLifecycleGraphApp(t, server)

	token, err := app.exchangeMetaMessengerAuthorizationCode(context.Background(), "authorization-code")
	require.NoError(t, err)
	assert.Equal(t, "user-token", token.AccessToken)
	assert.Equal(t, metaLifecycleTestAppID, captured.Get("client_id"))
	assert.Equal(t, metaLifecycleTestAppSecret, captured.Get("client_secret"))
	assert.Equal(t, "authorization-code", captured.Get("code"))
	assert.Empty(t, captured.Get("scope"))
	assert.Empty(t, captured.Get("redirect_uri"))
}

func TestMetaMessengerAuthenticatedGraphRequestsUseAppSecretProof(t *testing.T) {
	const accessToken = "server-only-provider-access-token"
	mac := hmac.New(sha256.New, []byte(metaLifecycleTestAppSecret))
	_, _ = mac.Write([]byte(accessToken))
	expectedProof := hex.EncodeToString(mac.Sum(nil))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "Bearer "+accessToken, request.Header.Get("Authorization"))
				require.NoError(t, request.ParseForm())
				assert.Equal(t, expectedProof, request.Form.Get("appsecret_proof"))
				assert.NotContains(t, request.URL.String(), metaLifecycleTestAppSecret)
				assert.NotContains(t, request.Form.Encode(), metaLifecycleTestAppSecret)
				for name, values := range request.Header {
					assert.NotContains(t, name+strings.Join(values, ","), metaLifecycleTestAppSecret)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			app := newMetaLifecycleGraphApp(t, server)
			var destination map[string]any
			err := app.doMetaMessengerGraphJSON(
				t.Context(), method, "me", url.Values{"fields": {"id"}},
				accessToken, &destination,
			)
			require.NoError(t, err)
			assert.Equal(t, true, destination["ok"])
		})
	}
}

func TestMetaMessengerTokenInspectionUsesExactScopesForGraphEdges(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		scopes     []string
		wantError  bool
		missingKey string
	}{
		{
			name: "minimum plus business edge without read engagement",
			scopes: []string{
				"public_profile", "pages_show_list", "pages_manage_metadata",
				"pages_messaging", "business_management",
			},
		},
		{
			name: "public profile is required",
			scopes: []string{
				"pages_show_list", "pages_manage_metadata", "pages_messaging",
				"business_management",
			},
			wantError:  true,
			missingKey: "public_profile",
		},
		{
			name: "business management is required for business ownership edges",
			scopes: []string{
				"public_profile", "pages_show_list", "pages_manage_metadata",
				"pages_messaging",
			},
			wantError:  true,
			missingKey: "business_management",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "/v25.0/debug_token", request.URL.Path)
				require.Equal(t, http.MethodPost, request.Method)
				require.NoError(t, request.ParseForm())
				assert.Equal(t, "user-token", request.Form.Get("input_token"))
				appAccessToken := metaLifecycleTestAppID + "|" + metaLifecycleTestAppSecret
				assert.Equal(t, "Bearer "+appAccessToken, request.Header.Get("Authorization"))
				mac := hmac.New(sha256.New, []byte(metaLifecycleTestAppSecret))
				_, _ = mac.Write([]byte(appAccessToken))
				assert.Equal(t, hex.EncodeToString(mac.Sum(nil)), request.Form.Get("appsecret_proof"))
				assert.Empty(t, request.URL.RawQuery)
				assert.NotContains(t, request.URL.String(), "user-token")
				assert.NotContains(t, request.URL.String(), metaLifecycleTestAppSecret)
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
					"is_valid": true,
					"app_id":   metaLifecycleTestAppID,
					"type":     metaMessengerTokenKindUser,
					"user_id":  metaLifecycleTestUserID,
					"scopes":   testCase.scopes,
				}})
			}))
			defer server.Close()

			inspection, err := newMetaLifecycleGraphApp(t, server).inspectMetaMessengerToken(
				context.Background(),
				"user-token",
				true,
			)
			if testCase.wantError {
				require.Error(t, err)
				assert.ErrorContains(t, err, testCase.missingKey)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, testCase.scopes, inspection.Scopes)
			assert.NotContains(t, inspection.Scopes, "pages_read_engagement")
		})
	}
}

func TestMetaMessengerTokenInspectionRequiresBISUByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"is_valid":true,"app_id":"100000000000001","type":"USER","user_id":"900000000000001","scopes":["public_profile","pages_show_list","pages_manage_metadata","pages_messaging","business_management"]}}`))
	}))
	defer server.Close()
	app := newMetaLifecycleGraphApp(t, server)
	app.Config.MetaMessenger.AllowDevelopmentUserToken = false

	_, err := app.inspectMetaMessengerToken(t.Context(), "user-token", true)
	require.Error(t, err)
	assert.ErrorContains(t, err, "Business Integration System User")
}

func TestMetaMessengerDiscoveryRequiresExactOwnedPageAndRequiredTasks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/me":
			_, _ = writer.Write([]byte(`{"id":"900000000000001","name":"Admin"}`))
		case "/v25.0/me/accounts":
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"700000000000001","name":"Owned","tasks":["PROFILE_PLUS_MESSAGING","MODERATE"],"access_token":"owned-token"},
				{"id":"700000000000002","name":"Client","tasks":["MESSAGING","MODERATE"],"access_token":"client-token"},
				{"id":"700000000000003","name":"No Messaging","tasks":["MODERATE"],"access_token":"limited-token"},
				{"id":"700000000000004","name":"No Moderation","tasks":["MESSAGING"],"access_token":"limited-token"}
			]}`))
		case "/v25.0/me/businesses":
			_, _ = writer.Write([]byte(`{"data":[{"id":"200000000000001","name":"Portfolio"}]}`))
		case "/v25.0/200000000000001/owned_pages":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001"},{"id":"700000000000003"},{"id":"700000000000004"}]}`))
		case "/v25.0/200000000000001/client_pages":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000002"}]}`))
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	app := newMetaLifecycleGraphApp(t, server)
	platform, _, pages, err := app.discoverMetaMessengerInventory(context.Background(), "user-token", metaMessengerTokenInspection{
		AppID: metaLifecycleTestAppID, Type: metaMessengerTokenKindUser,
		UserID: metaLifecycleTestUserID, Scopes: append([]string(nil), metaMessengerRequiredScopes...),
		CheckedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.Equal(t, metaLifecycleTestUserID, platform.UserID)
	byID := make(map[string]metaMessengerStoredPage)
	for _, page := range pages {
		byID[page.PageID] = page
	}
	assert.True(t, byID[metaLifecycleTestPageID].Selectable)
	plain, err := appcrypto.Decrypt(byID[metaLifecycleTestPageID].EncryptedPageToken, metaLifecycleTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "owned-token", plain)
	assert.False(t, byID["700000000000002"].Selectable)
	assert.Equal(t, metaMessengerDisabledClient, byID["700000000000002"].DisabledReason)
	assert.False(t, byID["700000000000003"].Selectable)
	assert.Equal(t, metaMessengerDisabledTask, byID["700000000000003"].DisabledReason)
	assert.False(t, byID["700000000000004"].Selectable)
	assert.Equal(t, metaMessengerDisabledTask, byID["700000000000004"].DisabledReason)
}

func TestMetaMessengerRequiredPageTasksDoNotAcceptPartialAuthority(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		tasks []string
		want  bool
	}{
		{"both exact tasks", []string{"MESSAGING", "MODERATE"}, true},
		{"profile plus messaging and moderation", []string{"PROFILE_PLUS_MESSAGING", "MODERATE"}, true},
		{"messaging only", []string{"MESSAGING"}, false},
		{"moderation only", []string{"MODERATE"}, false},
		{"manage is not an undocumented alias", []string{"MANAGE"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, metaMessengerHasRequiredPageTasks(testCase.tasks))
		})
	}
}

func TestMetaMessengerSelectionRechecksBothPageTasksImmediatelyBeforePersistence(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		tasks     []string
		wantValid bool
	}{
		{"both tasks remain", []string{"MESSAGING", "MODERATE"}, true},
		{"messaging was removed", []string{"MODERATE"}, false},
		{"moderation was removed", []string{"MESSAGING"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v25.0/me/accounts":
					_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{
						"id":           metaLifecycleTestPageID,
						"name":         "Owned Page",
						"tasks":        testCase.tasks,
						"access_token": "fresh-page-token",
					}}})
				case "/v25.0/200000000000001/owned_pages":
					_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Owned Page"}]}`))
				default:
					http.Error(writer, "unexpected", http.StatusNotFound)
				}
			}))
			defer server.Close()

			app := newMetaLifecycleGraphApp(t, server)
			inspection := metaMessengerTokenInspection{
				AppID: metaLifecycleTestAppID, Type: metaMessengerTokenKindUser,
				UserID: metaLifecycleTestUserID, Scopes: append([]string(nil), metaMessengerRequiredScopes...),
				CheckedAt: time.Now().UTC(),
			}
			selected := metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
				BusinessID: metaLifecycleTestBusinessID,
				PageID:     metaLifecycleTestPageID,
				Ownership:  metaMessengerOwnershipOwned,
				Selectable: true,
			}}
			fresh, err := app.revalidateMetaMessengerOwnedPage(
				context.Background(),
				"user-token",
				inspection,
				selected,
			)
			if !testCase.wantValid {
				require.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
				assert.Empty(t, fresh.EncryptedPageToken)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, testCase.tasks, fresh.Tasks)
			plain, decryptErr := appcrypto.Decrypt(fresh.EncryptedPageToken, metaLifecycleTestEncryptionKey)
			require.NoError(t, decryptErr)
			assert.Equal(t, "fresh-page-token", plain)
		})
	}
}

func TestMetaMessengerSubscriptionRequiresExactPlatformAppAndMessages(t *testing.T) {
	for _, testCase := range []struct {
		name, response string
		wantError      bool
	}{
		{"exact", `{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`, false},
		{"wrong app", `{"data":[{"id":"999999999999999","subscribed_fields":["messages"]}]}`, true},
		{"wrong field", `{"data":[{"id":"100000000000001","subscribed_fields":["feed"]}]}`, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPost {
					_, _ = writer.Write([]byte(`{"success":true}`))
					return
				}
				_, _ = writer.Write([]byte(testCase.response))
			}))
			defer server.Close()
			app := newMetaLifecycleGraphApp(t, server)
			err := app.subscribeMetaMessengerPage(context.Background(), metaLifecycleTestPageID, "page-token")
			if testCase.wantError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMetaMessengerSignedDeauthorizationRejectsTamperAndStalePayload(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sign := func(issuedAt time.Time) string {
		payload, err := json.Marshal(metaMessengerSignedRequestPayload{
			Algorithm: "HMAC-SHA256", IssuedAt: issuedAt.Unix(), UserID: metaLifecycleTestUserID,
		})
		require.NoError(t, err)
		encoded := base64.RawURLEncoding.EncodeToString(payload)
		mac := hmac.New(sha256.New, []byte(metaLifecycleTestAppSecret))
		_, _ = mac.Write([]byte(encoded))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + encoded
	}
	raw := sign(now)
	payload, digest, err := verifyMetaMessengerSignedRequest(raw, metaLifecycleTestAppSecret, now)
	require.NoError(t, err)
	assert.Equal(t, metaLifecycleTestUserID, payload.UserID)
	assert.NotEmpty(t, digest)
	_, _, err = verifyMetaMessengerSignedRequest(raw+"tamper", metaLifecycleTestAppSecret, now)
	assert.Error(t, err)
	_, _, err = verifyMetaMessengerSignedRequest(sign(now.Add(-11*time.Minute)), metaLifecycleTestAppSecret, now)
	assert.Error(t, err)
}

func TestMetaMessengerDeauthorizationVerifiesHMACBeforeRateLimiter(t *testing.T) {
	t.Parallel()

	var redisDials atomic.Int64
	redisClient := redis.NewClient(&redis.Options{
		Addr:       "redis.invalid:6379",
		MaxRetries: -1,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			redisDials.Add(1)
			return nil, errors.New("synthetic Redis outage")
		},
	})
	t.Cleanup(func() { _ = redisClient.Close() })
	app := &App{
		Config: &configpkg.Config{MetaMessenger: configpkg.MetaMessengerConfig{
			Enabled: true, AppID: metaLifecycleTestAppID, AppSecret: metaLifecycleTestAppSecret,
		}},
		Redis: redisClient,
		Log:   testutil.NopLogger(),
	}
	request := func(signedRequest string) *fastglue.Request {
		req := testutil.NewRequest(t)
		req.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
		req.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
		req.RequestCtx.Request.SetBodyString(url.Values{"signed_request": {signedRequest}}.Encode())
		return req
	}

	invalid := request("unsigned.invalid")
	require.NoError(t, app.DeauthorizeMetaMessenger(invalid))
	testutil.AssertErrorResponse(t, invalid, fasthttp.StatusBadRequest, "Invalid signed request")
	assert.Zero(t, redisDials.Load(), "unsigned traffic must not consume the authenticated rate gate")

	payload, err := json.Marshal(metaMessengerSignedRequestPayload{
		Algorithm: "HMAC-SHA256", IssuedAt: time.Now().UTC().Unix(), UserID: metaLifecycleTestUserID,
	})
	require.NoError(t, err)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(metaLifecycleTestAppSecret))
	_, _ = mac.Write([]byte(encoded))
	valid := request(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + encoded)
	require.NoError(t, app.DeauthorizeMetaMessenger(valid))
	testutil.AssertErrorResponse(t, valid, fasthttp.StatusServiceUnavailable, "Deauthorization protection is unavailable")
	assert.Positive(t, redisDials.Load(), "a verified event should reach the digest-isolated rate gate")
}

func TestMetaMessengerScheduledRevalidationChecksExactPageAndSubscription(t *testing.T) {
	var subscribed atomic.Bool
	subscribed.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/debug_token":
			if request.URL.Query().Get("input_token") == "page-token" {
				_, _ = writer.Write([]byte(`{"data":{"is_valid":true,"app_id":"100000000000001","type":"PAGE","user_id":"700000000000001"}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"data":{"is_valid":true,"app_id":"100000000000001","type":"USER","user_id":"900000000000001","scopes":["public_profile","pages_show_list","pages_manage_metadata","pages_messaging","business_management"]}}`))
		case "/v25.0/me":
			if request.Header.Get("Authorization") == "Bearer page-token" {
				_, _ = writer.Write([]byte(`{"id":"700000000000001","name":"Owned Page"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"id":"900000000000001","name":"Admin"}`))
		case "/v25.0/me/accounts":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Owned Page","tasks":["MESSAGING","MODERATE"],"access_token":"page-token"}]}`))
		case "/v25.0/200000000000001/owned_pages":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Owned Page"}]}`))
		case "/v25.0/700000000000001/subscribed_apps":
			if subscribed.Load() {
				_, _ = writer.Write([]byte(`{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`))
			} else {
				_, _ = writer.Write([]byte(`{"data":[]}`))
			}
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	app := newMetaLifecycleGraphApp(t, server)
	future := time.Now().UTC().Add(time.Hour)
	snapshot := metaMessengerRevalidationSnapshot{
		Account: models.ChannelAccount{
			ExternalAccountID: metaLifecycleTestPageID,
			Metadata: models.JSONB{
				"meta_authorizing_user_id":             metaLifecycleTestUserID,
				"meta_business_id":                     metaLifecycleTestBusinessID,
				metaMessengerAuthorizationTokenKindKey: metaMessengerTokenKindUser,
			},
		},
		OAuth:          models.ChannelCredential{ExpiresAt: &future},
		AuthorityToken: "user-token",
		PageToken:      "page-token",
	}
	outcome, reason := app.checkMetaMessengerOwnership(context.Background(), snapshot)
	assert.Empty(t, outcome)
	assert.Equal(t, "scheduled_graph_revalidation", reason)

	subscribed.Store(false)
	outcome, reason = app.checkMetaMessengerOwnership(context.Background(), snapshot)
	assert.Equal(t, metaregistry.OwnershipStale, outcome)
	assert.Equal(t, "messages_subscription_missing", reason)
}

func TestMetaMessengerRuntimeAllowlistBlocksEveryLeaseAndSchedulerQuarantinesWithoutGraph(t *testing.T) {
	db := testutil.SetupTestDB(t)
	pilot := testutil.CreateTestOrganization(t, db)
	removed := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(
		t, db, removed.ID, models.ChannelMessenger, "780000000000120",
	)
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = true
	accountConfig["ai_reply_enabled"] = true
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, removed.ID).
		Update("config", accountConfig).Error)

	app := metaRegistryTestApp(db, removed.ID)
	app.Config.MetaMessenger.AllowedOrganizationIDs = pilot.ID.String()
	for _, purpose := range []string{
		metaregistry.ResolvePurposeInbound,
		metaregistry.ResolvePurposeOutbound,
		metaregistry.ResolvePurposeWorker,
		metaregistry.ResolvePurposeHealth,
	} {
		t.Run(purpose, func(t *testing.T) {
			_, err := app.loadMetaRegistryBinding(metaregistry.ResolveRequest{
				Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
				Purpose: purpose,
			}, time.Now().UTC())
			require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
		})
	}

	var graphCalls atomic.Int64
	app.HTTPClient = &http.Client{Transport: metaMessengerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		graphCalls.Add(1)
		return nil, errors.New("Graph must not be called for a removed pilot workspace")
	})}
	app.revalidateOneMetaMessengerBinding(
		t.Context(), removed.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Zero(t, graphCalls.Load())

	var quarantined models.ChannelAccount
	require.NoError(t, db.First(&quarantined, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, quarantined.Status)
	assert.False(t, boolConfigValue(quarantined.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(quarantined.Config, "ai_reply_enabled"))
	assert.Equal(t, metaregistry.OwnershipStale, stringConfigValue(quarantined.Metadata, "meta_ownership_state"))
	assert.Equal(t, "organization_removed_from_runtime_allowlist", stringConfigValue(quarantined.Metadata, "meta_ownership_reason"))
}

func TestMetaMessengerApprovalRechecksExactMessagesSubscription(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(
		t, db, organization.ID, models.ChannelMessenger, metaLifecycleTestPageID,
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_subscription_state"] = "verified"
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = now.Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	configJSON := cloneJSONB(fixture.account.Config)
	configJSON["outbound_enabled"] = false
	configJSON["ai_reply_enabled"] = false
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": configJSON, "metadata": metadata,
		"last_health_check_at": now, "last_error": "",
	}).Error)

	var subscribed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/v25.0/"+metaLifecycleTestPageID+"/subscribed_apps", request.URL.Path)
		require.Contains(t, request.URL.Query().Get("fields"), "subscribed_fields")
		require.Equal(t, "Bearer provider-token-"+metaLifecycleTestPageID, request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		field := "feed"
		if subscribed.Load() {
			field = "messages"
		}
		_, _ = writer.Write([]byte(`{"data":[{"id":"123","subscribed_fields":["` + field + `"]}]}`))
	}))
	defer server.Close()
	app := metaRegistryTestApp(db, organization.ID)
	app.Config.MetaRegistry.Enabled = true
	app.Config.MetaMessenger.Enabled = true
	app.Config.MetaMessenger.ConfigID = "456"
	app.Config.MetaMessenger.AppSecret = metaLifecycleTestAppSecret
	app.Config.MetaMessenger.GraphAPIVersion = "v25.0"
	app.Config.MetaMessenger.GraphBaseURL = "https://graph.meta.test"
	app.Config.MetaMessenger.ReReplyBaseURL = "https://app.example.test"
	app.Config.MetaMessenger.RelayBaseURL = "https://relay.example.test"
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.meta.test": server,
	})

	snapshot, err := app.loadMetaMessengerRevalidationSnapshot(
		organization.ID, fixture.account.ID, now,
	)
	require.NoError(t, err)
	_, err = app.freshMetaMessengerSubscriptionApproval(t.Context(), snapshot)
	require.Error(t, err, "a removed messages subscription must stop approval")
	var pending models.ChannelAccount
	require.NoError(t, db.First(&pending, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, pending.Status)
	assert.False(t, boolConfigValue(pending.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(pending.Config, "ai_reply_enabled"))

	subscribed.Store(true)
	evidence, err := app.freshMetaMessengerSubscriptionApproval(t.Context(), snapshot)
	require.NoError(t, err)
	activated, err := app.activateMetaMessengerAccount(
		organization.ID, fixture.userID, fixture.account.ID,
		time.Now().UTC(), evidence,
	)
	require.NoError(t, err)
	assert.Equal(t, models.ChannelAccountStatusActive, activated.Status)
	assert.True(t, boolConfigValue(activated.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(activated.Config, "ai_reply_enabled"))
}

func TestClassifyMetaMessengerRevalidationError(t *testing.T) {
	for _, testCase := range []struct {
		name, wantOutcome, wantReason string
		err                           error
	}{
		{"revoked token", metaregistry.OwnershipRevoked, "provider_authorization_revoked", &metaMessengerProviderError{StatusCode: 401}},
		{"provider throttle", "transient", "provider_temporarily_unavailable", &metaMessengerProviderError{StatusCode: 429}},
		{"provider outage", "transient", "provider_temporarily_unavailable", &metaMessengerProviderError{StatusCode: 503}},
		{"transport", "transient", "provider_revalidation_failed", errors.New("network unavailable")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			outcome, reason := classifyMetaMessengerRevalidationError(testCase.err)
			assert.Equal(t, testCase.wantOutcome, outcome)
			assert.Equal(t, testCase.wantReason, reason)
		})
	}
}

func TestMetaMessengerDeauthorizationPartialFailureRetriesIdempotently(t *testing.T) {
	targets := []metaDeauthorizationTarget{
		{OrganizationID: uuid.New(), AccountID: uuid.New()},
		{OrganizationID: uuid.New(), AccountID: uuid.New()},
	}
	revoked := map[uuid.UUID]bool{}
	failFirst := true
	revoke := func(target metaDeauthorizationTarget) (bool, error) {
		if target.AccountID == targets[0].AccountID && failFirst {
			return false, errors.New("synthetic tenant mutation failure")
		}
		if revoked[target.AccountID] {
			return false, nil
		}
		revoked[target.AccountID] = true
		return true, nil
	}

	applied, err := processMetaDeauthorizationTargets(targets, revoke)
	require.Error(t, err)
	assert.Equal(t, 1, applied)
	assert.False(t, revoked[targets[0].AccountID])
	assert.True(t, revoked[targets[1].AccountID], "a failed earlier target must not starve later tenants")

	failFirst = false
	applied, err = processMetaDeauthorizationTargets(targets, revoke)
	require.NoError(t, err)
	assert.Equal(t, 1, applied, "the already-revoked target must not be mutated twice")
	assert.True(t, revoked[targets[0].AccountID])
	assert.True(t, revoked[targets[1].AccountID])
}

func TestMetaMessengerDeauthorizationJournalResumesStaleRetryAndCleansUpBoundedly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	now := time.Now().UTC().Truncate(time.Second)
	payload := metaMessengerSignedRequestPayload{
		Algorithm: "HMAC-SHA256", IssuedAt: now.Unix(), UserID: metaLifecycleTestUserID,
	}
	digest := strings.Repeat("a", sha256.Size*2)
	event, err := app.loadOrCreateMetaDeauthorizationEvent(
		digest, metaLifecycleTestAppID, payload, now,
	)
	require.NoError(t, err)
	assert.Equal(t, "verified", event.State)

	resumed, err := app.loadOrCreateMetaDeauthorizationEvent(
		digest, metaLifecycleTestAppID, payload, now.Add(15*time.Minute),
	)
	require.NoError(t, err, "an exact durable journal entry must outlive the initial freshness window")
	assert.Equal(t, event.Digest, resumed.Digest)
	lateUnjournaled, err := app.loadOrCreateMetaDeauthorizationEvent(
		strings.Repeat("b", sha256.Size*2), metaLifecycleTestAppID, payload, now.Add(15*time.Minute),
	)
	require.NoError(t, err, "a first-attempt database outage must not make an authentic retry unrecoverable")
	assert.Equal(t, "verified", lateUnjournaled.State)
	_, err = app.loadOrCreateMetaDeauthorizationEvent(
		strings.Repeat("9", sha256.Size*2), metaLifecycleTestAppID, payload,
		now.Add(metaDeauthorizationUnresolvedRetention+time.Second),
	)
	require.ErrorIs(t, err, errMetaDeauthorizationEventStale)

	require.NoError(t, app.completeMetaDeauthorizationEvent(resumed, now.Add(16*time.Minute)))
	completed, err := app.loadOrCreateMetaDeauthorizationEvent(
		digest, metaLifecycleTestAppID, payload, now.Add(20*time.Minute),
	)
	require.NoError(t, err)
	assert.Equal(t, "completed", completed.State)
	encoded, err := json.Marshal(completed)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(encoded), "journal identifiers must not reach API or log JSON")

	oldCompletedAt := now.Add(-metaDeauthorizationCompletedRetention - time.Hour)
	oldUnresolvedAt := now.Add(-metaDeauthorizationUnresolvedRetention - time.Hour)
	recentCompletedAt := now.Add(-time.Hour)
	journalFixtures := []models.MetaDeauthorizationEvent{
		{Digest: strings.Repeat("c", 64), PlatformAppID: metaLifecycleTestAppID, AuthorizingUserID: metaLifecycleTestUserID,
			IssuedAt: oldCompletedAt, VerifiedAt: oldCompletedAt, State: "completed", CompletedAt: &oldCompletedAt},
		{Digest: strings.Repeat("d", 64), PlatformAppID: metaLifecycleTestAppID, AuthorizingUserID: metaLifecycleTestUserID,
			IssuedAt: oldUnresolvedAt, VerifiedAt: oldUnresolvedAt, State: "verified"},
		{Digest: strings.Repeat("e", 64), PlatformAppID: metaLifecycleTestAppID, AuthorizingUserID: metaLifecycleTestUserID,
			IssuedAt: recentCompletedAt, VerifiedAt: recentCompletedAt, State: "completed", CompletedAt: &recentCompletedAt},
		{Digest: strings.Repeat("f", 64), PlatformAppID: metaLifecycleTestAppID, AuthorizingUserID: metaLifecycleTestUserID,
			IssuedAt: recentCompletedAt, VerifiedAt: recentCompletedAt, State: "verified"},
	}
	require.NoError(t, db.Create(&journalFixtures).Error)
	require.NoError(t, app.cleanupMetaDeauthorizationEvents(now))
	var remaining []string
	require.NoError(t, db.Model(&models.MetaDeauthorizationEvent{}).Order("digest").Pluck("digest", &remaining).Error)
	assert.NotContains(t, remaining, strings.Repeat("c", 64))
	assert.NotContains(t, remaining, strings.Repeat("d", 64))
	assert.Contains(t, remaining, strings.Repeat("e", 64))
	assert.Contains(t, remaining, strings.Repeat("f", 64))
}

func TestMetaMessengerDelayedOldDeauthorizationCannotRevokeReconnectGeneration(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000108")
	const reconnectBusinessID = "280000000000108"
	const reconnectAuthorizingUserID = "980000000000108"
	fixture.account.Metadata = cloneJSONB(fixture.account.Metadata)
	fixture.account.Metadata["meta_business_id"] = reconnectBusinessID
	fixture.account.Metadata["meta_authorizing_user_id"] = reconnectAuthorizingUserID
	require.NoError(t, root.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.account.OrganizationID).
		Update("metadata", fixture.account.Metadata).Error)
	oldEventIssuedAt := time.Now().UTC().Add(-time.Second)
	result, err := func() (metaRegistryProvisionResult, error) {
		var rotated metaRegistryProvisionResult
		err := root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
			var rotateErr error
			rotated, rotateErr = scoped.rotateMetaMessengerBinding(metaMessengerRotateInput{
				AccountID: fixture.account.ID, OrganizationID: fixture.account.OrganizationID,
				UserID: fixture.userID,
				Page: metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID: reconnectBusinessID,
					PageID:     fixture.account.ExternalAccountID, PageName: "Reconnected Page",
				}},
				Platform: metaMessengerPlatformUser{
					UserID:    reconnectAuthorizingUserID,
					TokenKind: metaMessengerTokenKindSystemUser,
				},
				Inspection: metaMessengerTokenInspection{
					CheckedAt: time.Now().UTC(), Scopes: append([]string(nil), metaMessengerRequiredScopes...),
				},
				PageToken: "new-page-token", AuthorityToken: "new-authority-token",
				SubscriptionOperationID:        uuid.New(),
				SubscriptionOperationExpiresAt: time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
			})
			return rotateErr
		})
		return rotated, err
	}()
	require.NoError(t, err)

	changed, err := root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{
			OrganizationID: fixture.account.OrganizationID,
			AccountID:      fixture.account.ID,
		},
		oldEventIssuedAt,
		"old-event-digest",
		time.Now().UTC(),
	)
	require.NoError(t, err)
	assert.False(t, changed)

	var account models.ChannelAccount
	require.NoError(t, root.DB.Preload("Credentials").First(&account, "id = ?", fixture.account.ID).Error)
	assert.NotEqual(t, metaregistry.OwnershipRevoked, stringConfigValue(account.Metadata, "meta_ownership_state"))
	var newOAuth models.ChannelCredential
	require.NoError(t, root.DB.First(&newOAuth, "id = ?", result.OAuthCredentialID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, newOAuth.Status)
	authorizedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
	)
	require.NoError(t, err)
	ambiguousEvent := authorizedAt.UTC().Truncate(time.Second)
	if credentialSecond := newOAuth.CreatedAt.UTC().Truncate(time.Second); credentialSecond.After(ambiguousEvent) {
		ambiguousEvent = credentialSecond
	}
	changed, err = root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{OrganizationID: fixture.account.OrganizationID, AccountID: fixture.account.ID},
		ambiguousEvent,
		"same-second-event-digest",
		ambiguousEvent.Add(time.Second),
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "ambiguous")
	assert.False(t, changed)
	require.NoError(t, root.DB.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
	assert.Equal(t, "same-second-event-digest", stringConfigValue(account.Metadata, metaDeauthorizationPendingDigestKey))

	snapshot, err := root.loadMetaMessengerRevalidationSnapshot(
		fixture.account.OrganizationID,
		fixture.account.ID,
		ambiguousEvent.Add(2*time.Second),
	)
	require.NoError(t, err)
	reconciledAt := ambiguousEvent.Add(3 * time.Second)
	require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		applied, applyErr := scoped.applyMetaRegistryMutation(metaregistry.MutationRequest{
			ChannelAccountID: snapshot.Account.ID,
			CredentialID:     snapshot.OAuth.ID, CredentialVersion: snapshot.OAuth.Version,
			WebhookCredentialID: snapshot.Webhook.ID, WebhookCredentialVersion: snapshot.Webhook.Version,
			Outcome: metaregistry.OwnershipVerified, Reason: "scheduled_graph_revalidation",
			CheckedAt: reconciledAt,
		}, metaregistry.OwnershipVerified)
		if applyErr != nil {
			return applyErr
		}
		if !applied {
			return errors.New("synthetic reconciliation was superseded")
		}
		return scoped.resolvePendingMetaDeauthorizationRevalidation(
			snapshot,
			metaregistry.OwnershipVerified,
			reconciledAt,
		)
	}))
	changed, err = root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{OrganizationID: fixture.account.OrganizationID, AccountID: fixture.account.ID},
		ambiguousEvent,
		"same-second-event-digest",
		reconciledAt.Add(time.Second),
	)
	require.NoError(t, err, "the exact reconciled event must become idempotently completable")
	assert.False(t, changed)
	require.NoError(t, root.DB.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.Equal(t, "awaiting_health", stringConfigValue(account.Metadata, "meta_activation_state"))
	assert.Nil(t, account.LastHealthCheckAt)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))

	genuineEvent := reconciledAt.Add(time.Second)
	changed, err = root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{OrganizationID: fixture.account.OrganizationID, AccountID: fixture.account.ID},
		genuineEvent,
		"genuine-event-digest",
		genuineEvent.Add(time.Second),
	)
	require.NoError(t, err)
	assert.True(t, changed)
	targets, err := root.resolveAllMetaDeauthorizationTargets(
		"123",
		stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
	)
	require.NoError(t, err)
	assert.Contains(t, targets, metaDeauthorizationTarget{
		OrganizationID: fixture.account.OrganizationID,
		AccountID:      fixture.account.ID,
	}, "a disconnected tombstone must remain resolvable after Redis completion failure")
	changed, err = root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{OrganizationID: fixture.account.OrganizationID, AccountID: fixture.account.ID},
		genuineEvent,
		"genuine-event-digest",
		genuineEvent.Add(2*time.Second),
	)
	require.NoError(t, err, "a Redis completion failure must be able to retry the disconnected tombstone")
	assert.False(t, changed)
}

func TestMetaMessengerDeauthorizationSettlesPreexistingRevokedTombstone(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000111")
	revokedAt := time.Now().UTC()
	require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		applied, err := scoped.applyMetaRegistryMutation(metaregistry.MutationRequest{
			ChannelAccountID: fixture.account.ID,
			CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
			WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
			Outcome: metaregistry.OwnershipRevoked, Reason: "scheduled_provider_revocation",
			CheckedAt: revokedAt,
		}, metaregistry.OwnershipRevoked)
		if err != nil {
			return err
		}
		if !applied {
			return errors.New("synthetic revocation was superseded")
		}
		return nil
	}))
	digest := "preexisting-tombstone-event"
	eventIssuedAt := revokedAt.Add(time.Second).Truncate(time.Second)
	changed, err := root.revokeMetaDeauthorizationTarget(
		metaDeauthorizationTarget{OrganizationID: fixture.account.OrganizationID, AccountID: fixture.account.ID},
		eventIssuedAt,
		digest,
		eventIssuedAt.Add(time.Second),
	)
	require.NoError(t, err)
	assert.False(t, changed)
	var account models.ChannelAccount
	require.NoError(t, root.DB.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
	assert.Equal(t, digest, stringConfigValue(account.Metadata, metaDeauthorizationEventDigestKey))
	assert.Equal(t, eventIssuedAt.Format(time.RFC3339), stringConfigValue(account.Metadata, metaDeauthorizationEventIssuedAtKey))
}

func TestMetaMessengerLifecycleUsesRealRedisForRateAndReplayFences(t *testing.T) {
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL is required for the real Redis lifecycle test")
	}
	app := &App{Redis: redisClient, Log: testutil.NopLogger()}
	organizationID := uuid.New()
	userID := uuid.New()
	rateKey := "meta-messenger:rate:start:" + organizationID.String() + ":" + userID.String()
	digest := "deauth-" + uuid.NewString()
	replayKey := "meta-messenger:deauth:replay:" + digest
	nonce := "nonce-" + uuid.NewString()
	sessionID := "session-" + uuid.NewString()
	startKey := metaMessengerStartStateKey(organizationID, userID, nonce)
	selectionKey := metaMessengerSelectionStateKey(organizationID, userID, sessionID)
	t.Cleanup(func() {
		_ = redisClient.Del(t.Context(), rateKey, replayKey, startKey, selectionKey).Err()
	})

	first := testutil.NewJSONRequest(t, map[string]any{})
	allowed, err := app.requireMetaMessengerRateLimit(first, organizationID, userID, "start", 1, time.Minute)
	require.NoError(t, err)
	require.True(t, allowed)
	second := testutil.NewJSONRequest(t, map[string]any{})
	allowed, err = app.requireMetaMessengerRateLimit(second, organizationID, userID, "start", 1, time.Minute)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, fasthttp.StatusTooManyRequests, testutil.GetResponseStatusCode(second))

	claim, err := app.acquireMetaMessengerDeauthorization(t.Context(), digest)
	require.NoError(t, err)
	require.Equal(t, metaDeauthorizationClaimAcquired, claim.Status)
	concurrent, err := app.acquireMetaMessengerDeauthorization(t.Context(), digest)
	require.NoError(t, err)
	require.Equal(t, metaDeauthorizationClaimProcessing, concurrent.Status)
	// A target lookup or mutation failure releases only the matching processing
	// lease, allowing Meta's retry to resume incomplete revocation immediately.
	require.NoError(t, app.releaseMetaMessengerDeauthorization(t.Context(), claim))
	retry, err := app.acquireMetaMessengerDeauthorization(t.Context(), digest)
	require.NoError(t, err)
	require.Equal(t, metaDeauthorizationClaimAcquired, retry.Status)
	require.NoError(t, app.completeMetaMessengerDeauthorization(t.Context(), retry))
	replayed, err := app.acquireMetaMessengerDeauthorization(t.Context(), digest)
	require.NoError(t, err)
	require.Equal(t, metaDeauthorizationClaimCompleted, replayed.Status)
	ttl, err := redisClient.TTL(t.Context(), replayKey).Result()
	require.NoError(t, err)
	require.GreaterOrEqual(t, ttl, 23*time.Hour)

	require.NoError(t, redisClient.Set(t.Context(), startKey, "start-state", time.Minute).Err())
	_, err = redisClient.GetDel(
		t.Context(),
		metaMessengerStartStateKey(uuid.New(), userID, nonce),
	).Result()
	require.ErrorIs(t, err, redis.Nil, "another workspace must not consume the launch nonce")
	value, err := redisClient.GetDel(t.Context(), startKey).Result()
	require.NoError(t, err)
	require.Equal(t, "start-state", value)
	_, err = redisClient.GetDel(t.Context(), startKey).Result()
	require.ErrorIs(t, err, redis.Nil, "the launch nonce must be one-time")

	require.NoError(t, redisClient.Set(t.Context(), selectionKey, "selection-state", time.Minute).Err())
	_, err = redisClient.GetDel(
		t.Context(),
		metaMessengerSelectionStateKey(organizationID, uuid.New(), sessionID),
	).Result()
	require.ErrorIs(t, err, redis.Nil, "another user must not consume the selection session")
	value, err = redisClient.GetDel(t.Context(), selectionKey).Result()
	require.NoError(t, err)
	require.Equal(t, "selection-state", value)
	_, err = redisClient.GetDel(t.Context(), selectionKey).Result()
	require.ErrorIs(t, err, redis.Nil, "the selection session must be one-time")
}
