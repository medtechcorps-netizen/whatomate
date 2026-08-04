package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// --- ExchangeToken Tests ---

const (
	embeddedSignupTestAppID     = "synthetic-embedded-signup-app"
	embeddedSignupTestAppSecret = "synthetic-embedded-signup-secret"
)

func TestApp_ExchangeToken_Success_AutoRegistration(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	// Use unique IDs to prevent conflicts with parallel tests
	phoneID := fmt.Sprintf("123456789%d", time.Now().UnixNano()%1000000)
	wabaID := fmt.Sprintf("987654321%d", time.Now().UnixNano()%1000000)
	cacheKey := "whatsapp:account:v2:" + phoneID
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, "stale-former-owner-entry", time.Hour).Err())
	var durableClaimObservedAtRegistration atomic.Bool
	var staleCacheObservedAtRegistration atomic.Bool

	// Mock Meta API server
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/oauth/access_token"):
			// Token exchange (query parameters are in r.URL.RawQuery, not path)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "EAABwzLixnjYBO1234567890",
			})
		case strings.Contains(path, "/debug_token"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"app_id":                 embeddedSignupTestAppID,
					"is_valid":               true,
					"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
					"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
					"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
				},
			})
		case strings.Contains(path, wabaID) && strings.HasSuffix(path, "/phone_numbers"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": phoneID}},
			})
		case strings.Contains(path, phoneID):
			if strings.HasSuffix(path, "/register") {
				var claimCount int64
				if err := app.DB.Model(&models.WhatsAppAccount{}).
					Where("phone_id = ? AND organization_id = ?", phoneID, org.ID).
					Count(&claimCount).Error; err == nil && claimCount == 1 {
					durableClaimObservedAtRegistration.Store(true)
				}
				exists, err := app.Redis.Exists(context.Background(), cacheKey).Result()
				if err != nil || exists != 0 {
					staleCacheObservedAtRegistration.Store(true)
				}
				// Registration
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			} else {
				// Phone info - use timestamp to ensure unique account names
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"verified_name":        fmt.Sprintf("Test Business %d", time.Now().UnixNano()),
					"display_phone_number": "+1234567890",
				})
			}
		case strings.Contains(path, wabaID):
			// Webhook subscription
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	// Override WhatsApp client to use test server
	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "test_auth_code_123",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.True(t, durableClaimObservedAtRegistration.Load(),
		"the ownership claim must be committed before registration starts")
	assert.False(t, staleCacheObservedAtRegistration.Load(),
		"the former-owner cache entry must be invalidated before registration starts")

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	accountMap, ok := resp.Data["account"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "active", accountMap["status"])
	assert.Equal(t, phoneID, accountMap["phone_id"])
	assert.Equal(t, wabaID, accountMap["business_id"])
	assert.NotEmpty(t, resp.Data["pin"]) // PIN should be returned

	// Verify account was created in database
	var account models.WhatsAppAccount
	err = app.DB.Where("phone_id = ? AND organization_id = ?", phoneID, org.ID).First(&account).Error
	require.NoError(t, err)
	assert.Equal(t, "active", account.Status)
	assert.NotEmpty(t, account.Pin)
	assert.True(t, crypto.IsEncrypted(account.AccessToken))
	assert.True(t, crypto.IsEncrypted(account.Pin))
	account.DecryptSecrets(app.Config.App.EncryptionKey)
	assert.Equal(t, "EAABwzLixnjYBO1234567890", account.AccessToken)

	// Verify audit log exists
	assert.Eventually(t, func() bool {
		var auditCount int64
		if err := app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ? AND action = ?", org.ID, "account", models.AuditActionCreated).Count(&auditCount).Error; err != nil {
			return false
		}
		return auditCount > 0
	}, 1*time.Second, 10*time.Millisecond)
}

func TestApp_ExchangeToken_Success_PendingRegistration(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	// Use unique IDs to prevent conflicts with parallel tests
	phoneID := fmt.Sprintf("223456789%d", time.Now().UnixNano()%1000000)
	wabaID := fmt.Sprintf("887654321%d", time.Now().UnixNano()%1000000)

	// Mock Meta API server - registration fails (PIN already exists)
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/oauth/access_token"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "test_token",
			})
		case strings.Contains(path, "/debug_token"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"app_id":                 embeddedSignupTestAppID,
					"is_valid":               true,
					"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
					"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
					"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
				},
			})
		case strings.Contains(path, wabaID) && strings.HasSuffix(path, "/phone_numbers"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": phoneID}},
			})
		case strings.Contains(path, phoneID):
			if strings.HasSuffix(path, "/register") {
				// Registration fails - PIN already exists
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
					Error: struct {
						Message      string `json:"message"`
						Type         string `json:"type"`
						Code         int    `json:"code"`
						ErrorSubcode int    `json:"error_subcode"`
						ErrorUserMsg string `json:"error_user_msg"`
						ErrorData    struct {
							Details string `json:"details"`
						} `json:"error_data"`
						FBTraceID string `json:"fbtrace_id"`
					}{
						Message: "Two-step verification is already enabled",
						Code:    33,
					},
				})
			} else {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"verified_name": fmt.Sprintf("Test Business %d", time.Now().UnixNano()),
				})
			}
		case strings.Contains(path, wabaID):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "test_code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)

	accountMap, ok := resp.Data["account"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "pending_registration", accountMap["status"])
	assert.Nil(t, resp.Data["pin"]) // No PIN when pending
}

func TestApp_ExchangeToken_ReconnectMetaRejectionRetainsPriorPIN(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "reconnect-phone-" + uuid.NewString()
	wabaID := "reconnect-waba-" + uuid.NewString()
	const priorPIN = "112233"
	encryptedPIN, err := crypto.Encrypt(priorPIN, app.Config.App.EncryptionKey)
	require.NoError(t, err)
	require.NoError(t, app.DB.Create(&models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Existing reconnect account " + uuid.NewString(),
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    "old-token",
		APIVersion:     "v21.0",
		Status:         "active",
		Pin:            encryptedPIN,
	}).Error)

	var subscribeCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth/access_token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token"})
		case strings.HasSuffix(path, "/debug_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"app_id": embeddedSignupTestAppID, "is_valid": true,
				"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
				"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
				"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
			}})
		case strings.HasSuffix(path, "/"+phoneID+"/register"):
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "Two-step verification is already enabled", "code": 33,
			}})
		case strings.HasSuffix(path, "/"+wabaID+"/subscribed_apps"):
			subscribeCalls.Add(1)
		case strings.HasSuffix(path, "/"+wabaID+"/phone_numbers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": phoneID}}})
		case strings.HasSuffix(path, "/"+phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": phoneID, "verified_name": "Reconnect Phone",
				"display_phone_number": "+60444444444", "code_verification_status": "VERIFIED",
				"platform_type": "CLOUD_API",
			})
		case strings.HasSuffix(path, "/"+wabaID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": wabaID, "name": "Reconnect WABA"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code": "reconnect-code", "phone_id": phoneID, "waba_id": wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.ExchangeToken(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Zero(t, subscribeCalls.Load(), "subscription must not run after registration rejection")

	var response struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.NotContains(t, response.Data, "pin")
	assert.Contains(t, response.Data["warning"], "Registration failed")

	var persisted models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&persisted).Error)
	persisted.DecryptSecrets(app.Config.App.EncryptionKey)
	assert.Equal(t, "active", persisted.Status)
	assert.Equal(t, priorPIN, persisted.Pin)
}

func TestApp_ExchangeToken_RejectsConcurrentReconnectBeforeMetaMutation(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "concurrent-reconnect-phone-" + uuid.NewString()
	wabaID := "concurrent-reconnect-waba-" + uuid.NewString()
	attemptID := uuid.New()
	attemptStartedAt := time.Now().UTC()
	record := models.WhatsAppAccount{
		OrganizationID:             org.ID,
		Name:                       "Reconnect already running " + uuid.NewString(),
		PhoneID:                    phoneID,
		BusinessID:                 wabaID,
		AccessToken:                "existing-token",
		APIVersion:                 "v21.0",
		Status:                     "pending_registration",
		ConnectionAttemptID:        &attemptID,
		ConnectionAttemptStartedAt: &attemptStartedAt,
	}
	require.NoError(t, app.DB.Create(&record).Error)

	var registerCalls atomic.Int32
	var subscribeCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth/access_token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token"})
		case strings.HasSuffix(path, "/debug_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"app_id": embeddedSignupTestAppID, "is_valid": true,
				"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
				"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
				"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
			}})
		case strings.HasSuffix(path, "/"+phoneID+"/register"):
			registerCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(path, "/"+wabaID+"/subscribed_apps"):
			subscribeCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(path, "/"+wabaID+"/phone_numbers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": phoneID}}})
		case strings.HasSuffix(path, "/"+phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": phoneID, "verified_name": "Concurrent Reconnect",
				"display_phone_number": "+60555555555", "code_verification_status": "VERIFIED",
				"platform_type": "CLOUD_API",
			})
		case strings.HasSuffix(path, "/"+wabaID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": wabaID, "name": "Concurrent Reconnect WABA"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code": "concurrent-reconnect-code", "phone_id": phoneID, "waba_id": wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already in progress")
	assert.Zero(t, registerCalls.Load())
	assert.Zero(t, subscribeCalls.Load())

	var persisted models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", record.ID).First(&persisted).Error)
	require.NotNil(t, persisted.ConnectionAttemptID)
	assert.Equal(t, attemptID, *persisted.ConnectionAttemptID)
	assert.Equal(t, "existing-token", persisted.AccessToken)
}

func TestApp_ExchangeToken_InvalidCode(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	// Mock Meta API server - invalid code
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Invalid authorization code",
				Code:    100,
			},
		})
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "invalid_code",
		"phone_id": "123456789",
		"waba_id":  "987654321",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	body := string(testutil.GetResponseBody(req))
	assert.Contains(t, body, "Meta authorization code exchange failed")
}

func TestApp_ExchangeToken_RejectsClassicSignupWithoutExactAssetsBeforeCodeExchange(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	var oauthCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth/access_token") {
			oauthCalls.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "code only", body: map[string]any{"code": "code-only"}},
		{name: "missing phone", body: map[string]any{"code": "code", "waba_id": "waba"}},
		{name: "missing WABA", body: map[string]any{"code": "code", "phone_id": "phone"}},
		{name: "whitespace IDs", body: map[string]any{"code": "code", "phone_id": "  ", "waba_id": "\t"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.NewJSONRequest(t, tc.body)
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.ExchangeToken(req))
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
		})
	}
	assert.Zero(t, oauthCalls.Load(), "invalid asset payloads must not consume the short-lived code")
}

func TestApp_ExchangeToken_MissingFields(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"phone_id": "123",
		"waba_id":  "456",
	}) // missing code
	testutil.SetAuthContext(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_ExchangeToken_CoexistenceSelectsVerifiedBusinessAppPhoneAcrossPages(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "coexistence-phone-" + uuid.NewString()
	otherPhoneID := "cloud-phone-" + uuid.NewString()
	wabaID := "coexistence-waba-" + uuid.NewString()
	var registerCalls atomic.Int32
	var subscribeCalls atomic.Int32

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth/access_token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "coexistence-token"})
		case strings.HasSuffix(path, "/debug_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"app_id": embeddedSignupTestAppID, "is_valid": true,
				"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
				"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
				"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
			}})
		case strings.HasSuffix(path, "/"+phoneID+"/register"):
			registerCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(path, "/"+wabaID+"/subscribed_apps"):
			subscribeCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(path, "/"+wabaID+"/phone_numbers"):
			assert.Contains(t, r.URL.Query().Get("fields"), "is_on_biz_app")
			assert.Contains(t, r.URL.Query().Get("fields"), "platform_type")
			if r.URL.Query().Get("after") == "page-2" {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{
					"id": phoneID, "display_phone_number": "+60111111111",
					"verified_name": "Coexistence Clinic", "is_on_biz_app": true,
					"platform_type": "CLOUD_API",
				}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id": otherPhoneID, "display_phone_number": "+60222222222",
					"verified_name": "Cloud Only", "is_on_biz_app": false,
					"platform_type": "CLOUD_API",
				}},
				"paging": map[string]any{
					"cursors": map[string]string{"after": "page-2"},
					"next":    "https://graph.facebook.com/next",
				},
			})
		case strings.HasSuffix(path, "/"+phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": phoneID, "verified_name": "Coexistence Clinic",
				"display_phone_number": "+60111111111", "is_on_biz_app": true,
				"platform_type": "CLOUD_API", "code_verification_status": "VERIFIED",
			})
		case strings.HasSuffix(path, "/"+wabaID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": wabaID, "name": "Coexistence WABA"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":         "coexistence-code",
		"waba_id":      wabaID,
		"signup_event": "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.ExchangeToken(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data struct {
			Account handlers.AccountResponse `json:"account"`
			Pin     string                   `json:"pin"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.Equal(t, phoneID, response.Data.Account.PhoneID)
	assert.Equal(t, "active", response.Data.Account.Status)
	assert.Empty(t, response.Data.Pin)
	assert.Zero(t, registerCalls.Load())
	assert.Equal(t, int32(1), subscribeCalls.Load())

	var persisted models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&persisted).Error)
	assert.True(t, persisted.IsSMB)
}

func TestApp_ExchangeToken_RejectsCrossWorkspacePhoneClaimBeforeMetaMutation(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	ownerOrg := testutil.CreateTestOrganization(t, app.DB)
	claimantOrg := testutil.CreateTestOrganization(t, app.DB)
	claimant := createAdminUser(t, app, claimantOrg.ID)
	phoneID := "owned-phone-" + uuid.NewString()
	wabaID := "owned-waba-" + uuid.NewString()
	require.NoError(t, app.DB.Create(&models.WhatsAppAccount{
		OrganizationID: ownerOrg.ID,
		Name:           "Existing phone owner " + uuid.NewString(),
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    "existing-token",
		APIVersion:     "v21.0",
		Status:         "active",
	}).Error)

	var registerCalls atomic.Int32
	var subscribeCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth/access_token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "claim-token"})
		case strings.HasSuffix(path, "/debug_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"app_id": embeddedSignupTestAppID, "is_valid": true,
				"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
				"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
				"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
			}})
		case strings.HasSuffix(path, "/"+phoneID+"/register"):
			registerCalls.Add(1)
		case strings.HasSuffix(path, "/"+wabaID+"/subscribed_apps"):
			subscribeCalls.Add(1)
		case strings.HasSuffix(path, "/"+wabaID+"/phone_numbers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": phoneID}}})
		case strings.HasSuffix(path, "/"+phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": phoneID, "verified_name": "Owned Phone", "display_phone_number": "+60333333333",
				"code_verification_status": "VERIFIED", "platform_type": "CLOUD_API",
			})
		case strings.HasSuffix(path, "/"+wabaID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": wabaID, "name": "Owned WABA"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code": "claim-code", "phone_id": phoneID, "waba_id": wabaID,
	})
	testutil.SetAuthContext(req, claimantOrg.ID, claimant.ID)
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
	assert.Zero(t, registerCalls.Load())
	assert.Zero(t, subscribeCalls.Load())

	var activeCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("phone_id = ?", phoneID).Count(&activeCount).Error)
	assert.Equal(t, int64(1), activeCount)
}

func TestApp_ExchangeToken_RejectsDuplicateAccountWebhookVerifyToken(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":                 "synthetic-embedded-signup-code",
		"phone_id":             "duplicate-token-phone-id",
		"waba_id":              "duplicate-token-waba-id",
		"webhook_verify_token": "must-be-configured-centrally",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "managed in Settings > Integrations")

	var accountCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, "duplicate-token-phone-id").
		Count(&accountCount).Error)
	assert.Zero(t, accountCount)
}

func TestApp_ExchangeToken_Unauthorized(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "test",
		"phone_id": "123",
		"waba_id":  "456",
	})
	// No auth context set

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
}

// --- RegisterPhoneNumber Tests ---

func TestApp_RegisterPhoneNumber_Success_WithPIN(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "register-with-pin-" + uuid.NewString()

	// Create account with pending_registration status
	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - RegisterPhone WithPIN",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    "test_token",
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)
	var pendingVisibleAtMeta atomic.Bool

	// Mock Meta API server
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			// Registration call
			assert.Equal(t, "/v21.0/"+phoneID+"/register", r.URL.Path)
			assert.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))

			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "654321", body["pin"])
			var pending models.WhatsAppAccount
			if err := app.DB.First(&pending, "id = ? AND organization_id = ?", account.ID, org.ID).Error; err == nil && pending.Status == "pending_registration" {
				pending.DecryptSecrets(app.Config.App.EncryptionKey)
				if pending.Pin == "654321" {
					pendingVisibleAtMeta.Store(true)
				}
			}

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		} else {
			// GetPhoneNumberInfo call — return non-SMB phone info
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":            phoneID,
				"platform_type": "CLOUD_API",
			})
		}
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"pin": "654321",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.True(t, resp.Data["success"].(bool))
	assert.Equal(t, "654321", resp.Data["pin"])

	// Verify account status updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "active", updated.Status)
	assert.True(t, crypto.IsEncrypted(updated.Pin))
	updated.DecryptSecrets(app.Config.App.EncryptionKey)
	assert.Equal(t, "654321", updated.Pin)
	assert.True(t, pendingVisibleAtMeta.Load(), "pending registration and exact PIN must be committed before the Meta call")

	// Verify audit log exists
	time.Sleep(50 * time.Millisecond)
	var auditCount int64
	require.NoError(t, app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ? AND action = ?", org.ID, "account", models.AuditActionUpdated).Count(&auditCount).Error)
	assert.Greater(t, auditCount, int64(0))
}

func TestApp_RegisterPhoneNumber_Success_GeneratedPIN(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "register-generated-pin-" + uuid.NewString()

	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - GeneratedPIN",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    "test_token",
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			// Registration call — validate the generated PIN
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Len(t, body["pin"], 6) // Generated PIN should be 6 digits

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		} else {
			// GetPhoneNumberInfo call — return non-SMB phone info
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":            phoneID,
				"platform_type": "CLOUD_API",
			})
		}
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		// No PIN provided - should generate one
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.True(t, resp.Data["success"].(bool))
	assert.NotEmpty(t, resp.Data["pin"])
	assert.Len(t, resp.Data["pin"].(string), 6)

	// Verify account status updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "active", updated.Status)
	assert.True(t, crypto.IsEncrypted(updated.Pin))
	updated.DecryptSecrets(app.Config.App.EncryptionKey)
	assert.Len(t, updated.Pin, 6)

	// Verify audit log exists
	time.Sleep(50 * time.Millisecond)
	var auditCount int64
	require.NoError(t, app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ? AND action = ?", org.ID, "account", models.AuditActionUpdated).Count(&auditCount).Error)
	assert.Greater(t, auditCount, int64(0))
}

func TestApp_RegisterPhoneNumber_RegistrationFailed(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	phoneID := "register-failure-" + uuid.NewString()

	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - RegFailed",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    "test_token",
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	// Mock Meta API server - registration fails
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Phone number must be verified before registration",
				Code:    368,
			},
		})
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"pin": "123456",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)

	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	body := string(testutil.GetResponseBody(req))
	assert.Contains(t, body, "Phone number must be verified")

	// Verify account status NOT updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "pending_registration", updated.Status)
}

func TestApp_RegisterPhoneNumber_AccountNotFound(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"pin": "123456",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func TestApp_RegisterPhoneNumber_InvalidID(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"pin": "123456",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", "not-a-uuid")

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_RegisterPhoneNumber_CrossOrgIsolation(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)
	user2 := createAdminUser(t, app, org2.ID)
	phoneID := "register-cross-org-" + uuid.NewString()

	// Create account in org1
	account := &models.WhatsAppAccount{
		OrganizationID: org1.ID,
		Name:           "Test Account - CrossOrg Isolation",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    "test_token",
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	// User from org2 tries to register org1's account
	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"pin": "123456",
	})
	testutil.SetAuthContext(req, org2.ID, user2.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}
