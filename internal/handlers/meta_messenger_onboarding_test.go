package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/fastglue"
)

const (
	metaMessengerTestAppID         = "100000000000001"
	metaMessengerTestAppSecret     = "server-only-medtech-test-secret"
	metaMessengerTestBusinessID    = "200000000000001"
	metaMessengerTestPageID        = "700000000000001"
	metaMessengerTestUserID        = "900000000000001"
	metaMessengerTestEncryptionKey = "meta-messenger-test-encryption-key-long-enough"
)

func newMetaMessengerGraphTestApp(t *testing.T, server *httptest.Server) *App {
	t.Helper()
	return &App{
		Config: &configpkg.Config{
			App: configpkg.AppConfig{
				Environment:   "test",
				EncryptionKey: metaMessengerTestEncryptionKey,
			},
			MetaMessengerOnboarding: configpkg.MetaMessengerOnboardingConfig{
				Enabled:             true,
				AppID:               metaMessengerTestAppID,
				ConfigID:            "1720929458946813",
				OwnerBusinessID:     "2018290039073161",
				AppSecret:           metaMessengerTestAppSecret,
				GraphAPIVersion:     "v25.0",
				GraphBaseURL:        "https://graph.meta.test",
				TrustedRelayBaseURL: "https://relay.example.test/meta",
			},
			MetaRelay: configpkg.MetaRelayConfig{
				BaseURL: "https://relay.example.test/meta",
			},
		},
		HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
			"https://graph.meta.test": server,
		}),
	}
}

func TestMetaMessengerCodeExchangeUsesFLfBContractWithoutScopeOrRedirect(t *testing.T) {
	var captured url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/v25.0/oauth/access_token", request.URL.Path)
		captured = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"user-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)

	token, err := app.exchangeMetaMessengerAuthorizationCode(context.Background(), "authorization-code")
	require.NoError(t, err)
	assert.Equal(t, "user-token", token.AccessToken)
	assert.Equal(t, metaMessengerTestAppID, captured.Get("client_id"))
	assert.Equal(t, metaMessengerTestAppSecret, captured.Get("client_secret"))
	assert.Equal(t, "authorization-code", captured.Get("code"))
	assert.Empty(t, captured.Get("scope"))
	assert.Empty(t, captured.Get("redirect_uri"))
}

func TestMetaMessengerTokenInspectionAcceptsUserAndBusinessIntegrationSystemUser(t *testing.T) {
	for _, tokenKind := range []string{
		metaMessengerTokenKindUser,
		metaMessengerTokenKindSystemUser,
	} {
		t.Run(tokenKind, func(t *testing.T) {
			scopesJSON, err := json.Marshal(metaMessengerRequiredScopes)
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "/v25.0/debug_token", request.URL.Path)
				assert.Equal(t, "authorization-token", request.URL.Query().Get("input_token"))
				assert.Equal(t, metaMessengerTestAppID+"|"+metaMessengerTestAppSecret, request.URL.Query().Get("access_token"))
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"data":{"app_id":"` + metaMessengerTestAppID +
					`","type":"` + tokenKind + `","is_valid":true,"user_id":"` +
					metaMessengerTestUserID + `","scopes":` + string(scopesJSON) + `}}`))
			}))
			defer server.Close()
			app := newMetaMessengerGraphTestApp(t, server)

			inspection, err := app.inspectMetaMessengerToken(
				context.Background(),
				"authorization-token",
				true,
			)
			require.NoError(t, err)
			assert.Equal(t, tokenKind, inspection.Type)
			assert.Equal(t, metaMessengerTestUserID, inspection.UserID)
			assert.ElementsMatch(t, metaMessengerRequiredScopes, inspection.Scopes)
		})
	}
}

func TestMetaMessengerPublicStartContractContainsNoSecretScopeOrRedirect(t *testing.T) {
	var response startMetaMessengerOnboardingResponse
	response.Provider = "meta"
	response.Channel = "messenger"
	response.Mode = metaMessengerOnboardingMode
	response.Nonce = "one-time-nonce"
	response.ExpiresAt = time.Now().UTC().Add(time.Minute)
	response.PublicConfig.AppID = metaMessengerTestAppID
	response.PublicConfig.ConfigID = "1720929458946813"
	response.PublicConfig.GraphAPIVersion = "v25.0"
	response.PublicConfig.ResponseType = "code"
	response.PublicConfig.OverrideDefaultResponseType = true

	raw, err := json.Marshal(response)
	require.NoError(t, err)
	serialized := string(raw)
	assert.NotContains(t, serialized, metaMessengerTestAppSecret)
	assert.NotContains(t, serialized, "scope")
	assert.NotContains(t, serialized, "redirect")
	assert.NotContains(t, serialized, "owner_business")
	assert.NotContains(t, serialized, "relay.example")
}

func TestMetaMessengerDiscoveryIntersectsAccessWithOwnedPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer user-token", request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/me":
			assert.Equal(t, "id,name", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"id":"900000000000001","name":"Platform Admin"}`))
		case "/v25.0/me/accounts":
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"700000000000001","name":"Owned Clinic","tasks":["PROFILE_PLUS_MESSAGING"],"access_token":"owned-page-token"},
				{"id":"700000000000002","name":"Client Clinic","tasks":["MESSAGING"],"access_token":"client-page-token"},
				{"id":"700000000000003","name":"Unverified Clinic","tasks":["MESSAGING"],"access_token":"unverified-page-token"},
				{"id":"700000000000004","name":"No Messaging Task","tasks":["CREATE_CONTENT"],"access_token":"limited-page-token"}
			]}`))
		case "/v25.0/me/businesses":
			_, _ = writer.Write([]byte(`{"data":[{"id":"200000000000001","name":"Clinic Portfolio","permitted_roles":["ADMIN"]}]}`))
		case "/v25.0/200000000000001/owned_pages":
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"700000000000001","name":"Owned Clinic"},
				{"id":"700000000000004","name":"No Messaging Task"}
			]}`))
		case "/v25.0/200000000000001/client_pages":
			assert.Equal(t, "id,name", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000002","name":"Client Clinic","permitted_tasks":["PROFILE_PLUS_MESSAGING"]}]}`))
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)
	checkedAt := time.Now().UTC().Truncate(time.Second)

	platform, businesses, pages, err := app.discoverMetaMessengerInventory(
		context.Background(),
		"user-token",
		metaMessengerTokenInspection{
			AppID:     metaMessengerTestAppID,
			Type:      "USER",
			UserID:    metaMessengerTestUserID,
			Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
			CheckedAt: checkedAt,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, metaMessengerTestUserID, platform.UserID)
	require.Len(t, businesses, 1)
	require.Len(t, pages, 4)

	byID := make(map[string]metaMessengerStoredPage, len(pages))
	for _, page := range pages {
		byID[page.PageID] = page
	}
	owned := byID[metaMessengerTestPageID]
	assert.True(t, owned.Selectable)
	assert.Equal(t, metaMessengerOwnershipOwned, owned.Ownership)
	assert.Equal(t, metaMessengerTestBusinessID, owned.BusinessID)
	assert.Equal(t, checkedAt, owned.OwnershipVerifiedAt)
	assert.True(t, appcrypto.IsEncrypted(owned.EncryptedPageToken))
	plaintext, decryptErr := appcrypto.Decrypt(owned.EncryptedPageToken, metaMessengerTestEncryptionKey)
	require.NoError(t, decryptErr)
	assert.Equal(t, "owned-page-token", plaintext)

	client := byID["700000000000002"]
	assert.False(t, client.Selectable)
	assert.Equal(t, metaMessengerOwnershipClient, client.Ownership)
	assert.Equal(t, metaMessengerDisabledClient, client.DisabledReason)
	assert.Equal(t, []string{"MESSAGING"}, client.Tasks)
	assert.Empty(t, client.EncryptedPageToken)
	unverified := byID["700000000000003"]
	assert.False(t, unverified.Selectable)
	assert.Equal(t, metaMessengerOwnershipUnverified, unverified.Ownership)
	assert.Equal(t, metaMessengerDisabledUnverified, unverified.DisabledReason)
	withoutMessaging := byID["700000000000004"]
	assert.False(t, withoutMessaging.Selectable)
	assert.Equal(t, metaMessengerDisabledTask, withoutMessaging.DisabledReason)
	assert.Empty(t, withoutMessaging.EncryptedPageToken)
}

func TestMetaMessengerSystemUserDiscoveryPinsClientBusinessWithoutUserEdges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer system-user-token", request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/me":
			assert.Equal(t, "id,client_business_id", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"id":"900000000000001","client_business_id":"200000000000001"}`))
		case "/v25.0/900000000000001/assigned_pages":
			assert.Equal(t, "id,name,tasks,access_token", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"700000000000001","name":"Owned Clinic","tasks":["PROFILE_PLUS_MESSAGING"],"access_token":"system-page-token"},
				{"id":"700000000000002","name":"Assigned Client Clinic","tasks":["MESSAGING"],"access_token":"client-page-token"},
				{"id":"700000000000004","name":"Limited Clinic","tasks":["CREATE_CONTENT"],"permitted_tasks":["PROFILE_PLUS_MESSAGING"],"access_token":"limited-token"},
				{"id":"700000000000005","name":"Unverified Assigned Clinic","tasks":["MESSAGING"],"access_token":"unverified-token"},
				{"id":"700000000000006","name":"Tokenless Assigned Clinic","tasks":["MESSAGING"]}
			]}`))
		case "/v25.0/200000000000001/owned_pages":
			assert.Equal(t, "id,name", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[
				{"id":"700000000000001","name":"Owned Clinic"},
				{"id":"700000000000004","name":"Limited Clinic","permitted_tasks":["PROFILE_PLUS_MESSAGING"]},
				{"id":"700000000000006","name":"Tokenless Assigned Clinic"},
				{"id":"700000000000007","name":"Unassigned Owned Clinic"}
			]}`))
		case "/v25.0/200000000000001/client_pages":
			assert.Equal(t, "id,name", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000002","name":"Client Clinic","permitted_tasks":["MESSAGING"]}]}`))
		default:
			t.Fatalf("system-user discovery unexpectedly requested %s", request.URL.Path)
		}
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)
	checkedAt := time.Now().UTC().Truncate(time.Second)

	platform, businesses, pages, err := app.discoverMetaMessengerInventory(
		context.Background(),
		"system-user-token",
		metaMessengerTokenInspection{
			AppID:     metaMessengerTestAppID,
			Type:      metaMessengerTokenKindSystemUser,
			UserID:    metaMessengerTestUserID,
			Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
			CheckedAt: checkedAt,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, metaMessengerTokenKindSystemUser, platform.TokenKind)
	assert.Equal(t, metaMessengerTestBusinessID, platform.ClientBusinessID)
	require.Equal(t, []metaMessengerBusinessSummary{{
		ID:   metaMessengerTestBusinessID,
		Name: "Business Portfolio " + metaMessengerTestBusinessID,
	}}, businesses)
	require.Len(t, pages, 5)
	byID := make(map[string]metaMessengerStoredPage, len(pages))
	for _, page := range pages {
		byID[page.PageID] = page
	}
	assert.True(t, byID[metaMessengerTestPageID].Selectable)
	assert.True(t, appcrypto.IsEncrypted(byID[metaMessengerTestPageID].EncryptedPageToken))
	assert.Equal(t, metaMessengerDisabledTask, byID["700000000000004"].DisabledReason)
	assert.False(t, byID["700000000000004"].Selectable)
	assert.Empty(t, byID["700000000000004"].EncryptedPageToken)
	assert.Equal(t, metaMessengerDisabledClient, byID["700000000000002"].DisabledReason)
	assert.Equal(t, []string{"MESSAGING"}, byID["700000000000002"].Tasks)
	assert.False(t, byID["700000000000002"].Selectable)
	assert.Empty(t, byID["700000000000002"].EncryptedPageToken)
	assert.NotContains(t, byID, "700000000000005")
	assert.Equal(t, metaMessengerDisabledTokenMissing, byID["700000000000006"].DisabledReason)
	assert.False(t, byID["700000000000006"].Selectable)
	assert.Empty(t, byID["700000000000006"].EncryptedPageToken)
	assert.Equal(t, metaMessengerDisabledAssignment, byID["700000000000007"].DisabledReason)
	assert.False(t, byID["700000000000007"].Selectable)
	assert.Empty(t, byID["700000000000007"].EncryptedPageToken)
}

func TestMetaMessengerHasMessagingTaskAcceptsOnlyKnownMessagingTasks(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		tasks []string
		want  bool
	}{
		{name: "legacy task", tasks: []string{"MESSAGING"}, want: true},
		{name: "new Pages experience task", tasks: []string{"PROFILE_PLUS_MESSAGING"}, want: true},
		{name: "normalizes case and whitespace", tasks: []string{" profile_plus_messaging "}, want: true},
		{name: "unrelated profile task", tasks: []string{"PROFILE_PLUS_CREATE_CONTENT"}},
		{name: "suffix match is rejected", tasks: []string{"NOT_PROFILE_PLUS_MESSAGING"}},
		{name: "empty task list"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, metaMessengerHasMessagingTask(testCase.tasks))
		})
	}
}

func TestSelectMetaMessengerOwnedPageRejectsClientAndMissingEvidence(t *testing.T) {
	encrypted, err := appcrypto.Encrypt("page-token", metaMessengerTestEncryptionKey)
	require.NoError(t, err)
	owned := metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: metaMessengerTestBusinessID,
			PageID:     metaMessengerTestPageID,
			Ownership:  metaMessengerOwnershipOwned,
			Selectable: true,
		},
		EncryptedPageToken:  encrypted,
		OwnershipVerifiedAt: time.Now().UTC(),
	}
	client := owned
	client.PageID = "700000000000002"
	client.Ownership = metaMessengerOwnershipClient
	client.Selectable = false
	missingEvidence := owned
	missingEvidence.PageID = "700000000000003"
	missingEvidence.OwnershipVerifiedAt = time.Time{}

	selected, err := selectMetaMessengerOwnedPage(
		[]metaMessengerStoredPage{client, missingEvidence, owned},
		metaMessengerTestBusinessID,
		metaMessengerTestPageID,
	)
	require.NoError(t, err)
	assert.Equal(t, metaMessengerTestPageID, selected.PageID)
	_, err = selectMetaMessengerOwnedPage(
		[]metaMessengerStoredPage{client},
		metaMessengerTestBusinessID,
		client.PageID,
	)
	assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
	_, err = selectMetaMessengerOwnedPage(
		[]metaMessengerStoredPage{missingEvidence},
		metaMessengerTestBusinessID,
		missingEvidence.PageID,
	)
	assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
}

func TestMetaMessengerSelectionFreshlyRevalidatesAccessAndOwnedPages(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		ownedPages string
		tasks      string
		wantError  bool
	}{
		{
			name:       "fresh exact intersection",
			ownedPages: `{"data":[{"id":"700000000000001","name":"Fresh Owned Clinic"}]}`,
			tasks:      `"MESSAGING"`,
		},
		{
			name:       "fresh New Pages Experience messaging task",
			ownedPages: `{"data":[{"id":"700000000000001","name":"Fresh Owned Clinic"}]}`,
			tasks:      `"PROFILE_PLUS_MESSAGING"`,
		},
		{
			name:       "ownership removed after inventory",
			ownedPages: `{"data":[]}`,
			tasks:      `"MESSAGING"`,
			wantError:  true,
		},
		{
			name:       "messaging task removed after inventory",
			ownedPages: `{"data":[{"id":"700000000000001","name":"Fresh Owned Clinic"}]}`,
			tasks:      `"CREATE_CONTENT"`,
			wantError:  true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v25.0/me/accounts":
					_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Fresh Clinic","tasks":[` + testCase.tasks + `],"access_token":"fresh-page-token"}]}`))
				case "/v25.0/200000000000001/owned_pages":
					_, _ = writer.Write([]byte(testCase.ownedPages))
				default:
					http.Error(writer, "unexpected", http.StatusNotFound)
				}
			}))
			defer server.Close()
			app := newMetaMessengerGraphTestApp(t, server)
			selected := metaMessengerStoredPage{
				metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID: metaMessengerTestBusinessID,
					PageID:     metaMessengerTestPageID,
					Ownership:  metaMessengerOwnershipOwned,
					Selectable: true,
				},
				OwnershipVerifiedAt: time.Now().UTC().Add(-time.Minute),
			}
			fresh, err := app.revalidateMetaMessengerOwnedPage(
				context.Background(),
				"user-token",
				metaMessengerTokenInspection{
					Type:      metaMessengerTokenKindUser,
					CheckedAt: time.Now().UTC(),
				},
				selected,
			)
			if testCase.wantError {
				assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "Fresh Owned Clinic", fresh.PageName)
			assert.WithinDuration(t, time.Now().UTC(), fresh.OwnershipVerifiedAt, time.Second)
			plaintext, decryptErr := appcrypto.Decrypt(
				fresh.EncryptedPageToken,
				metaMessengerTestEncryptionKey,
			)
			require.NoError(t, decryptErr)
			assert.Equal(t, "fresh-page-token", plaintext)
		})
	}
}

func TestMetaMessengerSystemUserSelectionRevalidatesDirectOwnedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/" + metaMessengerTestUserID + "/assigned_pages":
			assert.Equal(t, "id,name,tasks,access_token", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Assigned BISU Clinic","tasks":["PROFILE_PLUS_MESSAGING"],"access_token":"fresh-system-page-token"}]}`))
		case "/v25.0/" + metaMessengerTestBusinessID + "/owned_pages":
			assert.Equal(t, "id,name", request.URL.Query().Get("fields"))
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Owned BISU Clinic"}]}`))
		default:
			t.Fatalf("unexpected system-user revalidation path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)
	selected := metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: metaMessengerTestBusinessID,
			PageID:     metaMessengerTestPageID,
			Ownership:  metaMessengerOwnershipOwned,
			Selectable: true,
		},
		OwnershipVerifiedAt: time.Now().UTC().Add(-time.Minute),
	}
	fresh, err := app.revalidateMetaMessengerOwnedPage(
		context.Background(),
		"system-user-token",
		metaMessengerTokenInspection{
			Type:      metaMessengerTokenKindSystemUser,
			UserID:    metaMessengerTestUserID,
			CheckedAt: time.Now().UTC(),
		},
		selected,
	)
	require.NoError(t, err)
	assert.Equal(t, "Owned BISU Clinic", fresh.PageName)
	assert.Equal(t, []string{"PROFILE_PLUS_MESSAGING"}, fresh.Tasks)
	plaintext, decryptErr := appcrypto.Decrypt(fresh.EncryptedPageToken, metaMessengerTestEncryptionKey)
	require.NoError(t, decryptErr)
	assert.Equal(t, "fresh-system-page-token", plaintext)
}

func TestMetaMessengerSystemUserSelectionRequiresCurrentAssignmentOwnershipAndToken(t *testing.T) {
	testCases := []struct {
		name                 string
		assignedPages        string
		ownedPages           string
		granularScopeTargets map[string]map[string]struct{}
	}{
		{
			name:          "assignment removed",
			assignedPages: `{"data":[]}`,
			ownedPages:    `{"data":[{"id":"700000000000001","name":"Owned BISU Clinic"}]}`,
		},
		{
			name:          "ownership removed",
			assignedPages: `{"data":[{"id":"700000000000001","name":"Assigned BISU Clinic","tasks":["MESSAGING"],"access_token":"fresh-system-page-token"}]}`,
			ownedPages:    `{"data":[]}`,
		},
		{
			name:          "assigned token missing",
			assignedPages: `{"data":[{"id":"700000000000001","name":"Assigned BISU Clinic","tasks":["MESSAGING"]}]}`,
			ownedPages:    `{"data":[{"id":"700000000000001","name":"Owned BISU Clinic"}]}`,
		},
		{
			name:          "granular Page target removed",
			assignedPages: `{"data":[{"id":"700000000000001","name":"Assigned BISU Clinic","tasks":["MESSAGING"],"access_token":"fresh-system-page-token"}]}`,
			ownedPages:    `{"data":[{"id":"700000000000001","name":"Owned BISU Clinic"}]}`,
			granularScopeTargets: map[string]map[string]struct{}{
				"pages_messaging": {"700000000000099": {}},
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/v25.0/" + metaMessengerTestUserID + "/assigned_pages":
					_, _ = writer.Write([]byte(testCase.assignedPages))
				case "/v25.0/" + metaMessengerTestBusinessID + "/owned_pages":
					_, _ = writer.Write([]byte(testCase.ownedPages))
				default:
					t.Fatalf("unexpected system-user revalidation path %s", request.URL.Path)
				}
			}))
			defer server.Close()
			app := newMetaMessengerGraphTestApp(t, server)
			selected := metaMessengerStoredPage{
				metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID: metaMessengerTestBusinessID,
					PageID:     metaMessengerTestPageID,
					Ownership:  metaMessengerOwnershipOwned,
					Selectable: true,
				},
			}

			_, err := app.revalidateMetaMessengerOwnedPage(
				context.Background(),
				"system-user-token",
				metaMessengerTokenInspection{
					Type:                 metaMessengerTokenKindSystemUser,
					UserID:               metaMessengerTestUserID,
					GranularScopeTargets: testCase.granularScopeTargets,
				},
				selected,
			)
			assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
		})
	}
}

func TestMetaMessengerSystemUserSelectionRejectsAssignableButUngrantedMessagingTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v25.0/" + metaMessengerTestUserID + "/assigned_pages":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Assigned BISU Clinic","tasks":["CREATE_CONTENT"],"permitted_tasks":["PROFILE_PLUS_MESSAGING"],"access_token":"fresh-system-page-token"}]}`))
		case "/v25.0/" + metaMessengerTestBusinessID + "/owned_pages":
			_, _ = writer.Write([]byte(`{"data":[{"id":"700000000000001","name":"Owned BISU Clinic","permitted_tasks":["PROFILE_PLUS_MESSAGING"]}]}`))
		default:
			t.Fatalf("unexpected system-user revalidation path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)
	selected := metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: metaMessengerTestBusinessID,
			PageID:     metaMessengerTestPageID,
			Ownership:  metaMessengerOwnershipOwned,
			Selectable: true,
		},
	}

	_, err := app.revalidateMetaMessengerOwnedPage(
		context.Background(),
		"system-user-token",
		metaMessengerTokenInspection{
			Type:   metaMessengerTokenKindSystemUser,
			UserID: metaMessengerTestUserID,
		},
		selected,
	)
	assert.ErrorIs(t, err, errMetaMessengerSelectionInvalid)
}

func TestMetaMessengerSubscriptionRequiresExactConfiguredAppAndMessages(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		subscription string
		wantError    bool
	}{
		{
			name:         "exact app and messages",
			subscription: `{"data":[{"id":"100000000000001","subscribed_fields":["messages"]}]}`,
		},
		{
			name:         "different app cannot satisfy verification",
			subscription: `{"data":[{"id":"9999999999999999","subscribed_fields":["messages"]}]}`,
			wantError:    true,
		},
		{
			name:         "messages field is mandatory",
			subscription: `{"data":[{"id":"100000000000001","subscribed_fields":["feed"]}]}`,
			wantError:    true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "/v25.0/"+metaMessengerTestPageID+"/subscribed_apps", request.URL.Path)
				assert.Equal(t, "Bearer page-token", request.Header.Get("Authorization"))
				assert.Empty(t, request.URL.Query().Get("access_token"))
				writer.Header().Set("Content-Type", "application/json")
				switch request.Method {
				case http.MethodPost:
					assert.NoError(t, request.ParseForm())
					assert.Equal(t, "messages", request.Form.Get("subscribed_fields"))
					_, _ = writer.Write([]byte(`{"success":true}`))
				case http.MethodGet:
					assert.Equal(t, "id,name,subscribed_fields", request.URL.Query().Get("fields"))
					_, _ = writer.Write([]byte(testCase.subscription))
				default:
					http.Error(writer, "unexpected", http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()
			app := newMetaMessengerGraphTestApp(t, server)

			err := app.subscribeMetaMessengerPage(
				context.Background(),
				metaMessengerTestPageID,
				"page-token",
			)
			if testCase.wantError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMissingMetaMessengerScopesNamesEachRequiredPermission(t *testing.T) {
	for _, requiredScope := range metaMessengerRequiredScopes {
		granted := make(map[string]struct{}, len(metaMessengerRequiredScopes)-1)
		for _, scope := range metaMessengerRequiredScopes {
			if scope != requiredScope {
				granted[scope] = struct{}{}
			}
		}
		assert.Equal(t, []string{requiredScope}, missingMetaMessengerScopes(granted))
	}
}

func TestMetaMessengerExplicitEmptyGranularTargetsFailClosed(t *testing.T) {
	inspection := metaMessengerTokenInspection{
		GranularScopeTargets: map[string]map[string]struct{}{
			"pages_messaging": {},
		},
	}
	assert.False(t, inspection.targetAllowed("pages_messaging", metaMessengerTestPageID))
	assert.True(t, inspection.targetAllowed("pages_show_list", metaMessengerTestPageID))
}

func TestMetaMessengerProviderErrorNeverIncludesProviderMessageOrSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-FB-Request-ID", "request-123")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"message":"leaked page-token and server-only-medtech-test-secret","type":"OAuthException","code":190,"error_subcode":463,"fbtrace_id":"trace-456"}}`))
	}))
	defer server.Close()
	app := newMetaMessengerGraphTestApp(t, server)

	_, err := app.exchangeMetaMessengerAuthorizationCode(context.Background(), "authorization-code")
	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, "code 190")
	assert.Contains(t, message, "trace trace-456")
	assert.Contains(t, message, "request request-123")
	assert.NotContains(t, message, "leaked")
	assert.NotContains(t, message, "page-token")
	assert.NotContains(t, message, metaMessengerTestAppSecret)
	assert.False(t, strings.Contains(strings.ToLower(message), "message"))
}

func TestMetaMessengerOnboardingDirectConfigIsFailClosedInProduction(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, testCase := range []struct {
		name      string
		baseURL   string
		wantError bool
	}{
		{name: "exact origin still staging only", baseURL: metaMessengerProductionGraphOrigin, wantError: true},
		{name: "alternate host", baseURL: "https://graph.example.test", wantError: true},
		{name: "explicit port", baseURL: "https://graph.facebook.com:443", wantError: true},
		{name: "trailing slash", baseURL: "https://graph.facebook.com/", wantError: true},
		{name: "path", baseURL: "https://graph.facebook.com/custom", wantError: true},
		{name: "query", baseURL: "https://graph.facebook.com?x=1", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newMetaMessengerGraphTestApp(t, server)
			app.Config.App.Environment = "production"
			app.Config.MetaMessengerOnboarding.GraphBaseURL = testCase.baseURL
			_, err := app.metaMessengerGraphEndpoint("me")
			if testCase.wantError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestMetaMessengerPendingSelectionResumesWithoutDuplicatingRelaySecrets(t *testing.T) {
	app := newMetaMessengerPersistenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceChannelAccounts+":"+models.ActionWrite,
	)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, admin.ID, "omnichannel.enabled")

	for _, testCase := range []struct {
		name              string
		state             string
		tokenKind         string
		clientBusinessID  string
		expireActiveLease bool
	}{
		{name: "expired verifying subscription", state: metaMessengerVerifyingSubscription, tokenKind: metaMessengerTokenKindUser, expireActiveLease: true},
		{name: "subscription failed", state: metaMessengerSubscriptionFailed, tokenKind: metaMessengerTokenKindSystemUser, clientBusinessID: metaMessengerTestBusinessID},
		{name: "awaiting relay registry", state: metaMessengerAwaitingRegistryState, tokenKind: metaMessengerTokenKindUser},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pageID := testutil.UniqueNumericID(t, "7")
			firstPage := newMetaMessengerPersistencePage(t, pageID, "first-page-token")
			firstInspection := newMetaMessengerAuthorizationInspection(testCase.tokenKind)
			first, err := app.persistMetaMessengerPendingAccount(
				newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
				org.ID,
				admin.ID,
				firstPage,
				metaMessengerTokenInspection{CheckedAt: time.Now().UTC()},
				firstInspection,
				testCase.clientBusinessID,
			)
			require.NoError(t, err)
			if testCase.state != metaMessengerVerifyingSubscription {
				_, err = app.finalizeMetaMessengerPendingAccount(
					org.ID,
					admin.ID,
					first.ID,
					testCase.state,
					testCase.state == metaMessengerAwaitingRegistryState,
					"test subscription failure",
				)
				require.NoError(t, err)
			}
			require.Len(t, first.Credentials, 2)
			firstWebhook := credentialByKind(t, first.Credentials, models.ChannelCredentialKindWebhook)
			if testCase.expireActiveLease {
				first.Metadata = cloneJSONB(first.Metadata)
				first.Metadata[metaMessengerSubscriptionOperationExpiresKey] = time.Now().UTC().
					Add(-time.Minute).
					Format(time.RFC3339Nano)
				require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", first.ID, org.ID).
					Update("metadata", first.Metadata).Error)
			}

			secondPage := newMetaMessengerPersistencePage(t, pageID, "second-page-token")
			secondInspection := newMetaMessengerAuthorizationInspection(testCase.tokenKind)
			secondInspection.CheckedAt = firstInspection.CheckedAt.Add(time.Minute)
			resumed, err := app.persistMetaMessengerPendingAccount(
				newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
				org.ID,
				admin.ID,
				secondPage,
				metaMessengerTokenInspection{CheckedAt: secondInspection.CheckedAt},
				secondInspection,
				testCase.clientBusinessID,
			)
			require.NoError(t, err)
			assert.Equal(t, first.ID, resumed.ID)
			assert.Equal(t, metaMessengerVerifyingSubscription, stringConfigValue(resumed.Metadata, "onboarding_state"))
			assert.Equal(t, testCase.tokenKind, stringConfigValue(resumed.Metadata, "meta_token_kind"))
			assert.Equal(t, testCase.clientBusinessID, stringConfigValue(resumed.Metadata, "meta_client_business_id"))

			var accountCount int64
			require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND external_account_id = ?", org.ID, pageID).
				Count(&accountCount).Error)
			assert.EqualValues(t, 1, accountCount)

			var credentials []models.ChannelCredential
			require.NoError(t, app.DB.Where(
				"organization_id = ? AND channel_account_id = ?",
				org.ID,
				first.ID,
			).Order("kind, version").Find(&credentials).Error)
			var webhooks, oauth []models.ChannelCredential
			for _, credential := range credentials {
				switch credential.Kind {
				case models.ChannelCredentialKindWebhook:
					webhooks = append(webhooks, credential)
				case models.ChannelCredentialKindOAuth:
					oauth = append(oauth, credential)
				}
			}
			require.Len(t, webhooks, 1)
			assert.Equal(t, firstWebhook.ID, webhooks[0].ID)
			assert.Equal(t, firstWebhook.CredentialBlob, webhooks[0].CredentialBlob)
			require.Len(t, oauth, 2)
			assert.Equal(t, models.ChannelCredentialStatusRevoked, oauth[0].Status)
			assert.Equal(t, models.ChannelCredentialStatusActive, oauth[1].Status)
			assert.Equal(t, 2, oauth[1].Version)
			encryptedToken, ok := oauth[1].CredentialBlob["access_token"].(string)
			require.True(t, ok)
			plaintext, decryptErr := appcrypto.Decrypt(encryptedToken, integrationTestEncryptionKey)
			require.NoError(t, decryptErr)
			assert.Equal(t, "second-page-token", plaintext)
		})
	}
}

func TestMetaMessengerPendingSelectionRejectsActiveLeaseWhenReviewFlagIsFalse(t *testing.T) {
	app := newMetaMessengerPersistenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceChannelAccounts+":"+models.ActionWrite,
	)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, admin.ID, "omnichannel.enabled")
	pageID := testutil.UniqueNumericID(t, "7")
	firstPage := newMetaMessengerPersistencePage(t, pageID, "first-page-token")
	authorization := newMetaMessengerAuthorizationInspection(metaMessengerTokenKindUser)
	first, err := app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
		org.ID,
		admin.ID,
		firstPage,
		metaMessengerTokenInspection{CheckedAt: authorization.CheckedAt},
		authorization,
		"",
	)
	require.NoError(t, err)
	originalOperation, err := metaMessengerSubscriptionOperationFromAccount(first)
	require.NoError(t, err)
	require.Equal(t, metaMessengerSubscriptionSubscribePending, originalOperation.State)
	require.True(t, originalOperation.ExpiresAt.After(time.Now().UTC()))
	originalOAuth := credentialByKind(t, first.Credentials, models.ChannelCredentialKindOAuth)

	first.Config = cloneJSONB(first.Config)
	first.Config["review_only"] = false
	require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", first.ID, org.ID).
		Update("config", first.Config).Error)

	secondPage := newMetaMessengerPersistencePage(t, pageID, "must-not-be-stored")
	_, err = app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
		org.ID,
		admin.ID,
		secondPage,
		metaMessengerTokenInspection{CheckedAt: time.Now().UTC()},
		newMetaMessengerAuthorizationInspection(metaMessengerTokenKindUser),
		"",
	)
	require.ErrorIs(t, err, errMetaMessengerPageBound)

	var persisted models.ChannelAccount
	require.NoError(t, app.DB.First(&persisted, "id = ? AND organization_id = ?", first.ID, org.ID).Error)
	currentOperation, err := metaMessengerSubscriptionOperationFromAccount(&persisted)
	require.NoError(t, err)
	assert.Equal(t, originalOperation, currentOperation)
	assert.Equal(t, false, persisted.Config["review_only"])

	var oauth []models.ChannelCredential
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ?",
		org.ID,
		first.ID,
		models.ChannelCredentialKindOAuth,
	).Order("version").Find(&oauth).Error)
	require.Len(t, oauth, 1)
	assert.Equal(t, originalOAuth.ID, oauth[0].ID)
	assert.Equal(t, originalOAuth.Version, oauth[0].Version)
	assert.Equal(t, originalOAuth.Status, oauth[0].Status)
	assert.Equal(t, originalOAuth.CredentialBlob, oauth[0].CredentialBlob)
	encryptedToken, ok := oauth[0].CredentialBlob["access_token"].(string)
	require.True(t, ok)
	plaintext, decryptErr := appcrypto.Decrypt(encryptedToken, integrationTestEncryptionKey)
	require.NoError(t, decryptErr)
	assert.Equal(t, "first-page-token", plaintext)
}

func TestMetaMessengerResumeStopsAfterProtectedInventoryRecognition(t *testing.T) {
	app := newMetaMessengerPersistenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, models.ResourceChannelAccounts+":"+models.ActionWrite)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, admin.ID, "omnichannel.enabled")
	pageID := testutil.UniqueNumericID(t, "7")
	page := newMetaMessengerPersistencePage(t, pageID, "first-page-token")
	authorization := newMetaMessengerAuthorizationInspection(metaMessengerTokenKindUser)
	account, err := app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
		org.ID,
		admin.ID,
		page,
		metaMessengerTokenInspection{CheckedAt: authorization.CheckedAt},
		authorization,
		"",
	)
	require.NoError(t, err)
	app.Config.MetaRelay.ExpectedAccountsJSON = trustedMetaMessengerInventoryJSON(org.ID, account.ID, page.PageID)

	retryPage := newMetaMessengerPersistencePage(t, pageID, "must-not-be-stored")
	_, err = app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, org.ID, admin.ID),
		org.ID,
		admin.ID,
		retryPage,
		metaMessengerTokenInspection{CheckedAt: time.Now().UTC()},
		newMetaMessengerAuthorizationInspection(metaMessengerTokenKindUser),
		"",
	)
	assert.ErrorIs(t, err, errMetaMessengerPageBound)
	var oauthCount int64
	require.NoError(t, app.DB.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ? AND kind = ?", account.ID, models.ChannelCredentialKindOAuth).
		Count(&oauthCount).Error)
	assert.EqualValues(t, 1, oauthCount)
}

func TestMetaMessengerGlobalPageUniquenessBlocksAnotherWorkspace(t *testing.T) {
	app := newMetaMessengerPersistenceTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	adminA := integrationTestUser(t, app, orgA.ID, models.ResourceChannelAccounts+":"+models.ActionWrite)
	adminB := integrationTestUser(t, app, orgB.ID, models.ResourceChannelAccounts+":"+models.ActionWrite)
	enableBookingCommerceTestEntitlement(t, app.DB, orgA.ID, adminA.ID, "omnichannel.enabled")
	enableBookingCommerceTestEntitlement(t, app.DB, orgB.ID, adminB.ID, "omnichannel.enabled")
	pageID := testutil.UniqueNumericID(t, "7")
	page := newMetaMessengerPersistencePage(t, pageID, "workspace-a-page-token")
	authorization := newMetaMessengerAuthorizationInspection(metaMessengerTokenKindUser)
	_, err := app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, orgA.ID, adminA.ID),
		orgA.ID,
		adminA.ID,
		page,
		metaMessengerTokenInspection{CheckedAt: authorization.CheckedAt},
		authorization,
		"",
	)
	require.NoError(t, err)

	page = newMetaMessengerPersistencePage(t, pageID, "workspace-b-page-token")
	_, err = app.persistMetaMessengerPendingAccount(
		newMetaMessengerPersistenceRequest(t, orgB.ID, adminB.ID),
		orgB.ID,
		adminB.ID,
		page,
		metaMessengerTokenInspection{CheckedAt: authorization.CheckedAt},
		authorization,
		"",
	)
	require.Error(t, err)
	var count int64
	require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
		Where("channel = ? AND provider = ? AND external_account_id = ?", models.ChannelMessenger, "relay", pageID).
		Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestMetaMessengerBusinessConsistencyNeverTrustsTenantConfig(t *testing.T) {
	app := newMetaMessengerPersistenceTestApp(t)
	managed := &models.ChannelAccount{
		Channel: models.ChannelMessenger,
		Config:  models.JSONB{"meta_business_id": "999999999999999"},
		Metadata: models.JSONB{
			"management_mode":            metaMessengerManagementMode,
			"meta_business_id":           metaMessengerTestBusinessID,
			"ownership_evidence_version": "owned_pages_v1",
			"ownership_verified_at":      time.Now().UTC().Format(time.RFC3339Nano),
		},
	}
	businessID, err := app.metaMessengerExistingBusinessID(managed)
	require.NoError(t, err)
	assert.Equal(t, metaMessengerTestBusinessID, businessID)

	legacyID := uuid.New()
	orgID := uuid.New()
	legacy := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: legacyID},
		OrganizationID:    orgID,
		Channel:           models.ChannelMessenger,
		Provider:          "relay",
		ExternalAccountID: "700000000000099",
		Config: models.JSONB{
			"relay_url":        "https://relay.example.test/meta/v1/accounts/messenger/700000000000099",
			"meta_business_id": "999999999999999",
		},
	}
	app.Config.MetaRelay.ExpectedAccountsJSON = fmt.Sprintf(`{"accounts":[{
		"organization_id":%q,"meta_business_id":%q,"channel":"messenger",
		"external_account_id":"700000000000099","rereply_account_id":%q
	}]}`, orgID.String(), metaMessengerTestBusinessID, legacyID.String())
	businessID, err = app.metaMessengerExistingBusinessID(legacy)
	require.NoError(t, err)
	assert.Equal(t, metaMessengerTestBusinessID, businessID)
}

func newMetaMessengerPersistenceTestApp(t *testing.T) *App {
	t.Helper()
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.App.Environment = "test"
	app.Config.MetaMessengerOnboarding = configpkg.MetaMessengerOnboardingConfig{
		Enabled:             true,
		AppID:               metaMessengerTestAppID,
		ConfigID:            "1720929458946813",
		OwnerBusinessID:     "2018290039073161",
		AppSecret:           metaMessengerTestAppSecret,
		GraphAPIVersion:     "v25.0",
		GraphBaseURL:        "https://graph.meta.test",
		TrustedRelayBaseURL: "https://relay.example.test/meta",
	}
	app.Config.MetaRelay = configpkg.MetaRelayConfig{
		BaseURL:              "https://relay.example.test/meta",
		ExpectedAccountsJSON: `{"accounts":[]}`,
		ProviderProofSecret:  "test-meta-provider-proof-secret-at-least-32-bytes",
	}
	return app
}

func newMetaMessengerPersistenceRequest(t *testing.T, orgID, userID uuid.UUID) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(request, orgID, userID)
	return request
}

func newMetaMessengerPersistencePage(t *testing.T, pageID, token string) metaMessengerStoredPage {
	t.Helper()
	encrypted, err := appcrypto.Encrypt(token, integrationTestEncryptionKey)
	require.NoError(t, err)
	return metaMessengerStoredPage{
		metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID:   metaMessengerTestBusinessID,
			BusinessName: "Clinic Portfolio",
			PageID:       pageID,
			PageName:     "Clinic Page",
			Ownership:    metaMessengerOwnershipOwned,
			Selectable:   true,
			Tasks:        []string{"MESSAGING"},
		},
		EncryptedPageToken:  encrypted,
		OwnershipVerifiedAt: time.Now().UTC(),
	}
}

func newMetaMessengerAuthorizationInspection(tokenKind string) metaMessengerTokenInspection {
	return metaMessengerTokenInspection{
		AppID:     metaMessengerTestAppID,
		Type:      tokenKind,
		UserID:    metaMessengerTestUserID,
		Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
		CheckedAt: time.Now().UTC(),
	}
}

func credentialByKind(
	t *testing.T,
	credentials []models.ChannelCredential,
	kind models.ChannelCredentialKind,
) models.ChannelCredential {
	t.Helper()
	for _, credential := range credentials {
		if credential.Kind == kind {
			return credential
		}
	}
	require.FailNow(t, "credential kind not found", string(kind))
	return models.ChannelCredential{}
}

func trustedMetaMessengerInventoryJSON(orgID, accountID uuid.UUID, pageID string) string {
	return fmt.Sprintf(`{
		"messenger_app":{"app_id":%q,"owner_business_id":%q},
		"accounts":[{
			"organization_id":%q,"meta_business_id":%q,"channel":"messenger",
			"external_account_id":%q,"rereply_account_id":%q
		}]
	}`,
		metaMessengerTestAppID,
		"2018290039073161",
		orgID.String(),
		metaMessengerTestBusinessID,
		pageID,
		accountID.String(),
	)
}
