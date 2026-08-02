package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestValidateWhatsAppAccountContractProviderLookupCases(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		configure     func(*whatsappContractMeta)
		errorContains string
	}{
		{name: "valid relationship"},
		{
			name: "mismatched phone and WABA",
			configure: func(meta *whatsappContractMeta) {
				meta.listedPhoneID = "different-phone"
			},
			errorContains: "does not belong",
		},
		{
			name: "relationship lookup failure",
			configure: func(meta *whatsappContractMeta) {
				meta.lookupStatus = http.StatusBadGateway
			},
			errorContains: "failed to verify phone-business relationship",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			meta := newWhatsAppContractMeta(t, "unit-phone", "unit-waba")
			if testCase.configure != nil {
				testCase.configure(meta)
			}
			app := &App{
				Log:      testutil.NopLogger(),
				WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), meta.server.URL),
			}

			_, err := app.validateWhatsAppAccountContract(
				context.Background(),
				"unit-phone",
				"unit-waba",
				"synthetic-unit-token",
				"v21.0",
			)
			if testCase.errorContains == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, testCase.errorContains)
		})
	}
}

type whatsappContractMeta struct {
	mu                 sync.Mutex
	server             *httptest.Server
	phoneID            string
	wabaID             string
	listedPhoneID      string
	lookupStatus       int
	subscriptionStatus int
	hits               map[string]int
	onSubscribe        func()
}

func newWhatsAppContractMeta(t *testing.T, phoneID, wabaID string) *whatsappContractMeta {
	t.Helper()
	meta := &whatsappContractMeta{
		phoneID:            phoneID,
		wabaID:             wabaID,
		listedPhoneID:      phoneID,
		lookupStatus:       http.StatusOK,
		subscriptionStatus: http.StatusOK,
		hits:               make(map[string]int),
	}
	meta.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta.mu.Lock()
		meta.hits[r.URL.Path]++
		meta.mu.Unlock()

		switch {
		case r.URL.Path == "/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "synthetic-embedded-token"})
		case r.URL.Path == "/debug_token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"app_id":   "synthetic-meta-app",
					"is_valid": true,
					"scopes": []string{
						"business_management",
						"whatsapp_business_management",
						"whatsapp_business_messaging",
					},
					"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
					"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
				},
			})
		case strings.HasSuffix(r.URL.Path, "/"+meta.wabaID+"/subscribed_apps"):
			if meta.onSubscribe != nil {
				meta.onSubscribe()
			}
			if meta.subscriptionStatus != http.StatusOK {
				w.WriteHeader(meta.subscriptionStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "synthetic subscription failure", "code": 190}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(r.URL.Path, "/"+meta.wabaID+"/phone_numbers"):
			if meta.lookupStatus != http.StatusOK {
				w.WriteHeader(meta.lookupStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "synthetic lookup failure", "code": 100}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{
					"id":                   meta.listedPhoneID,
					"display_phone_number": "+60123456789",
					"verified_name":        "Synthetic Clinic",
					"quality_rating":       "GREEN",
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/"+meta.wabaID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": meta.wabaID, "name": "Synthetic WABA"})
		case strings.HasSuffix(r.URL.Path, "/"+meta.phoneID+"/register"):
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(r.URL.Path, "/"+meta.phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                       meta.phoneID,
				"display_phone_number":     "+60123456789",
				"verified_name":            "Synthetic Clinic",
				"code_verification_status": "VERIFIED",
				"account_mode":             "LIVE",
				"quality_rating":           "GREEN",
				"platform_type":            "CLOUD_API",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(meta.server.Close)
	return meta
}

func (m *whatsappContractMeta) hit(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

func newWhatsAppContractApp(t *testing.T, meta *whatsappContractMeta) *App {
	t.Helper()
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.WhatsApp.AppID = "synthetic-meta-app"
	app.Config.WhatsApp.AppSecret = "synthetic-meta-app-secret"
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.Config.WhatsApp.BaseURL = meta.server.URL
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, meta.server.URL)
	return app
}

func contractWriter(t *testing.T, app *App, orgID uuid.UUID) *models.User {
	t.Helper()
	return integrationTestUser(t, app, orgID, "accounts:read", "accounts:write")
}

func createContractAccountRequest(t *testing.T, app *App, orgID, userID uuid.UUID, name, phoneID, wabaID, token string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         name,
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": token,
	})
	testutil.SetAuthContext(req, orgID, userID)
	require.NoError(t, app.CreateAccount(req))
	return req
}

func TestCreateAccountValidatesRelationshipPersistsPendingThenActivates(t *testing.T) {
	phoneID := "contract-phone-valid"
	wabaID := "contract-waba-valid"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	var sawPendingEncrypted bool
	meta.onSubscribe = func() {
		var account models.WhatsAppAccount
		if err := app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&account).Error; err == nil {
			sawPendingEncrypted = account.Status == "pending_subscription" &&
				appcrypto.IsEncrypted(account.AccessToken)
		}
	}

	req := createContractAccountRequest(t, app, org.ID, user.ID, "Valid Contract", phoneID, wabaID, "synthetic-valid-token")
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var envelope struct {
		Data AccountResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	assert.Equal(t, "active", envelope.Data.Status)
	assert.Empty(t, envelope.Data.Warning)
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "synthetic-valid-token")
	assert.True(t, sawPendingEncrypted, "subscription must see an existing encrypted pending row")
	assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/phone_numbers"))
	assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.True(t, appcrypto.IsEncrypted(stored.AccessToken))
}

func TestCreateAccountRejectsPhoneWABAMismatchBeforePersistence(t *testing.T) {
	phoneID := "contract-phone-mismatch"
	wabaID := "contract-waba-mismatch"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.listedPhoneID = "different-phone"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := createContractAccountRequest(t, app, org.ID, user.ID, "Mismatch Contract", phoneID, wabaID, "synthetic-mismatch-token")
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "does not belong")
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestCreateAccountRejectsRelationshipLookupFailureBeforePersistence(t *testing.T) {
	phoneID := "contract-phone-lookup"
	wabaID := "contract-waba-lookup"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.lookupStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := createContractAccountRequest(t, app, org.ID, user.ID, "Lookup Contract", phoneID, wabaID, "synthetic-lookup-token")
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "failed to verify phone-business relationship")
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestCreateAccountPersistsSubscriptionFailureHonestly(t *testing.T) {
	phoneID := "contract-phone-subscription"
	wabaID := "contract-waba-subscription"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.subscriptionStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	var sawPending bool
	meta.onSubscribe = func() {
		var account models.WhatsAppAccount
		if err := app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&account).Error; err == nil {
			sawPending = account.Status == "pending_subscription"
		}
	}

	req := createContractAccountRequest(t, app, org.ID, user.ID, "Subscription Contract", phoneID, wabaID, "synthetic-subscription-token")
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var envelope struct {
		Data AccountResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	assert.Equal(t, "subscription_failed", envelope.Data.Status)
	assert.Contains(t, envelope.Data.Warning, "Webhook subscription failed")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "synthetic-subscription-token")
	assert.True(t, sawPending)

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.Equal(t, "subscription_failed", stored.Status)
	assert.True(t, appcrypto.IsEncrypted(stored.AccessToken))
}

func TestUpdateAccountContractChangeValidatesThenResubscribes(t *testing.T) {
	phoneID := "contract-phone-update"
	wabaID := "contract-waba-update"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.subscriptionStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	oldToken, err := appcrypto.Encrypt("synthetic-old-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Existing Contract",
		PhoneID:        "old-phone",
		BusinessID:     "old-waba",
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(account).Error)

	var sawPendingNewTuple bool
	meta.onSubscribe = func() {
		var persisted models.WhatsAppAccount
		if err := app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&persisted).Error; err == nil {
			sawPendingNewTuple = persisted.PhoneID == phoneID &&
				persisted.BusinessID == wabaID &&
				persisted.Status == "pending_subscription" &&
				appcrypto.IsEncrypted(persisted.AccessToken)
		}
	}

	req := testutil.NewJSONRequest(t, map[string]any{
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "synthetic-updated-token",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var envelope struct {
		Data AccountResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	assert.Equal(t, "subscription_failed", envelope.Data.Status)
	assert.Contains(t, envelope.Data.Warning, "Webhook subscription failed")
	assert.True(t, sawPendingNewTuple)

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "subscription_failed", stored.Status)
	assert.Equal(t, phoneID, stored.PhoneID)
	assert.Equal(t, wabaID, stored.BusinessID)
	assert.True(t, appcrypto.IsEncrypted(stored.AccessToken))
}

func TestEmbeddedSignupSuppliedIDsCannotBypassRelationshipValidation(t *testing.T) {
	phoneID := "contract-phone-embedded"
	wabaID := "contract-waba-embedded"
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.listedPhoneID = "different-embedded-phone"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-authorization-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "does not belong")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestWhatsAppContractTestFixtureRoutesRemainExplicit(t *testing.T) {
	// Guard against accidental test-server fallthrough masking a missing Meta
	// endpoint in the contract tests above.
	meta := newWhatsAppContractMeta(t, "fixture-phone", "fixture-waba")
	resp, err := http.Get(fmt.Sprintf("%s/unexpected", meta.server.URL))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
