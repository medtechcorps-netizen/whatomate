package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_RequestCoexistenceSync(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		syncType whatsapp.CoexistenceSyncType
		request  func(context.Context, *whatsapp.Client, *whatsapp.Account) (*whatsapp.CoexistenceSyncResponse, error)
	}{
		{
			name:     "contacts",
			syncType: whatsapp.CoexistenceSyncContacts,
			request: func(ctx context.Context, contextClient *whatsapp.Client, account *whatsapp.Account) (*whatsapp.CoexistenceSyncResponse, error) {
				return contextClient.RequestCoexistenceContactSync(ctx, account)
			},
		},
		{
			name:     "history",
			syncType: whatsapp.CoexistenceSyncHistory,
			request: func(ctx context.Context, contextClient *whatsapp.Client, account *whatsapp.Account) (*whatsapp.CoexistenceSyncResponse, error) {
				return contextClient.RequestCoexistenceHistorySync(ctx, account)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v25.0/123456789012345/smb_app_data", r.URL.Path)
				assert.Empty(t, r.URL.RawQuery)
				assert.Equal(t, "Bearer secret-access-token", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				var body map[string]string
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, map[string]string{
					"messaging_product": "whatsapp",
					"sync_type":         string(tt.syncType),
				}, body)

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{
					"messaging_product": "whatsapp",
					"request_id":        "sync-request-123",
				})
			}))
			defer server.Close()

			client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
			account := &whatsapp.Account{
				PhoneID:     "123456789012345",
				APIVersion:  "v25.0",
				AccessToken: "secret-access-token",
			}

			response, err := tt.request(testutil.TestContext(t), client, account)
			require.NoError(t, err)
			assert.Equal(t, "whatsapp", response.MessagingProduct)
			assert.Equal(t, "sync-request-123", response.RequestID)
		})
	}
}

func TestClient_RequestCoexistenceSyncRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	validAccount := whatsapp.Account{
		PhoneID:     "123456789012345",
		APIVersion:  "v25.0",
		AccessToken: "secret-access-token",
	}
	tests := []struct {
		name       string
		account    *whatsapp.Account
		syncType   whatsapp.CoexistenceSyncType
		wantErrMsg string
	}{
		{name: "nil account", account: nil, syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "account is required"},
		{name: "phone path injection", account: cloneCoexistenceAccount(validAccount, func(a *whatsapp.Account) { a.PhoneID = "123/smb_app_data" }), syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "invalid phone_id"},
		{name: "phone surrounding whitespace", account: cloneCoexistenceAccount(validAccount, func(a *whatsapp.Account) { a.PhoneID = " 123456789012345" }), syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "invalid phone_id"},
		{name: "API version injection", account: cloneCoexistenceAccount(validAccount, func(a *whatsapp.Account) { a.APIVersion = "v25.0/123" }), syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "invalid WhatsApp API version"},
		{name: "API version surrounding whitespace", account: cloneCoexistenceAccount(validAccount, func(a *whatsapp.Account) { a.APIVersion = " v25.0" }), syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "invalid WhatsApp API version"},
		{name: "missing token", account: cloneCoexistenceAccount(validAccount, func(a *whatsapp.Account) { a.AccessToken = " " }), syncType: whatsapp.CoexistenceSyncContacts, wantErrMsg: "access token is required"},
		{name: "unknown sync type", account: &validAccount, syncType: "contacts", wantErrMsg: "invalid coexistence sync type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.RequestCoexistenceSync(testutil.TestContext(t), tt.account, tt.syncType)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErrMsg)
		})
	}
	assert.Zero(t, requests.Load())
}

func TestClient_RequestCoexistenceSyncRequiresRequestID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp"}`))
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	_, err := client.RequestCoexistenceHistorySync(testutil.TestContext(t), &whatsapp.Account{
		PhoneID:     "123456789012345",
		APIVersion:  "v25.0",
		AccessToken: "secret-access-token",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "without a request_id")
}

func TestClient_RequestCoexistenceSyncRejectsInvalidResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		response        string
		wantErrContains string
	}{
		{name: "malformed JSON", response: `{`, wantErrContains: "failed to parse response"},
		{name: "missing product", response: `{"request_id":"request-123"}`, wantErrContains: "unexpected messaging product"},
		{name: "blank product", response: `{"messaging_product":" ","request_id":"request-123"}`, wantErrContains: "unexpected messaging product"},
		{name: "unexpected product", response: `{"messaging_product":"instagram","request_id":"request-123"}`, wantErrContains: "unexpected messaging product"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()

			client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL+"/")
			_, err := client.RequestCoexistenceContactSync(testutil.TestContext(t), &whatsapp.Account{
				PhoneID:     "123456789012345",
				APIVersion:  "v25.0",
				AccessToken: "secret-access-token",
			})
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErrContains)
		})
	}
}

func TestClient_RequestCoexistenceSyncPreservesDefiniteProviderRejection(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"sync is unavailable","code":100,"is_transient":false}}`))
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	_, err := client.RequestCoexistenceContactSync(testutil.TestContext(t), &whatsapp.Account{
		PhoneID:     "123456789012345",
		APIVersion:  "v25.0",
		AccessToken: "secret-access-token",
	})
	require.Error(t, err)
	assert.True(t, whatsapp.IsDefiniteProviderRejection(err))
}

func TestClient_RequestCoexistenceSyncDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()

	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret-access-token", r.Header.Get("Authorization"))
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), redirector.URL)
	_, err := client.RequestCoexistenceContactSync(testutil.TestContext(t), &whatsapp.Account{
		PhoneID:     "123456789012345",
		APIVersion:  "v25.0",
		AccessToken: "secret-access-token",
	})
	require.Error(t, err)
	assert.Zero(t, destinationRequests.Load(), "access token must not be replayed to redirects")
}

func cloneCoexistenceAccount(account whatsapp.Account, mutate func(*whatsapp.Account)) *whatsapp.Account {
	mutate(&account)
	return &account
}
