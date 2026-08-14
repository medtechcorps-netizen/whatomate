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
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// --- ExchangeToken Tests ---

const (
	embeddedSignupTestAppID     = "synthetic-embedded-signup-app"
	embeddedSignupTestAppSecret = "synthetic-embedded-signup-secret"
)

func setEmbeddedSignupAuth(req *fastglue.Request, orgID, userID uuid.UUID) {
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetHeader(req, "X-Organization-ID", orgID.String())
}

func TestApp_ExchangeToken_Success_AutoRegistration(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	// Use unique IDs to prevent conflicts with parallel tests
	phoneID := fmt.Sprintf("123456789%d", time.Now().UnixNano()%1000000)
	wabaID := fmt.Sprintf("987654321%d", time.Now().UnixNano()%1000000)
	var durableClaimObserved atomic.Bool
	var cacheEvictedObserved atomic.Bool

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
					"app_id":   embeddedSignupTestAppID,
					"is_valid": true,
					"scopes":   []string{"whatsapp_business_management", "whatsapp_business_messaging"},
					"granular_scopes": []map[string]any{{
						"scope":      "whatsapp_business_management",
						"target_ids": []string{wabaID},
					}},
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
				var registerBody map[string]string
				_ = json.NewDecoder(r.Body).Decode(&registerBody)
				var claimed models.WhatsAppAccount
				if err := app.DB.Where("organization_id = ? AND BTRIM(phone_id) = BTRIM(?)", org.ID, phoneID).
					First(&claimed).Error; err == nil {
					decrypted := claimed
					decrypted.DecryptSecrets(app.Config.App.EncryptionKey)
					durableClaimObserved.Store(
						claimed.Status == "pending_registration" &&
							crypto.IsEncrypted(claimed.AccessToken) &&
							crypto.IsEncrypted(claimed.Pin) &&
							len(registerBody["pin"]) == 6 &&
							decrypted.Pin == registerBody["pin"],
					)
				}
				if exists, err := app.Redis.Exists(context.Background(), "whatsapp:account:"+phoneID).Result(); err == nil {
					cacheEvictedObserved.Store(exists == 0)
				}
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
	require.NoError(t, app.Redis.Set(context.Background(), "whatsapp:account:"+phoneID, "stale-account", time.Hour).Err())

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "test_auth_code_123",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	setEmbeddedSignupAuth(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

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
	assert.NotContains(t, resp.Data, "pin")
	assert.True(t, durableClaimObserved.Load(), "register must observe a committed encrypted claim and the exact stored PIN")
	assert.True(t, cacheEvictedObserved.Load(), "the phone cache must be evicted before Meta registration")

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
					"app_id":   embeddedSignupTestAppID,
					"is_valid": true,
					"scopes":   []string{"whatsapp_business_management", "whatsapp_business_messaging"},
					"granular_scopes": []map[string]any{{
						"scope":      "whatsapp_business_management",
						"target_ids": []string{wabaID},
					}},
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
	setEmbeddedSignupAuth(req, org.ID, user.ID)

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
	assert.NotContains(t, resp.Data, "pin")

	var pending models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&pending).Error)
	assert.True(t, crypto.IsEncrypted(pending.Pin), "failed registration must retain the recoverable PIN encrypted at rest")
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
	setEmbeddedSignupAuth(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	body := string(testutil.GetResponseBody(req))
	assert.Contains(t, body, "Meta authorization code exchange failed")
}

func TestApp_ExchangeToken_Success_CodeOnly_Discovery(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	phoneID := fmt.Sprintf("333456789%d", time.Now().UnixNano()%1000000)
	wabaID := fmt.Sprintf("777654321%d", time.Now().UnixNano()%1000000)

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/oauth/access_token"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "discovery_token",
			})
		case strings.Contains(path, "/debug_token"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": whatsapp.TokenDebugInfo{
					AppID:   embeddedSignupTestAppID,
					IsValid: true,
					Scopes: []string{
						"whatsapp_business_management",
						"whatsapp_business_messaging",
					},
					ExpiresAt:           time.Now().UTC().Add(2 * time.Hour).Unix(),
					DataAccessExpiresAt: time.Now().UTC().Add(time.Hour).Unix(),
					GranularScopes: []struct {
						Scope     string   `json:"scope"`
						TargetIds []string `json:"target_ids,omitempty"`
					}{
						{
							Scope:     "whatsapp_business_management",
							TargetIds: []string{wabaID},
						},
					},
				},
			})
		case strings.Contains(path, wabaID) && strings.Contains(path, "/phone_numbers"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(whatsapp.WABAPhoneNumbersResponse{
				Data: []whatsapp.WABAPhoneNumber{
					{
						ID:                 phoneID,
						DisplayPhoneNumber: "+1999999999",
						VerifiedName:       "Discovered Phone",
						QualityRating:      "GREEN",
					},
				},
			})
		case strings.Contains(path, "/me/accounts"):
			// Fallback mock (should not be reached if debug_token works)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(whatsapp.SharedWABAResponse{})
		case strings.Contains(path, phoneID) && strings.Contains(path, "/register"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.Contains(path, wabaID) && strings.Contains(path, "/subscribed_apps"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.Contains(path, phoneID): // Phone Info
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"verified_name":        "Discovered Phone",
				"display_phone_number": "+1999999999",
			})
		case strings.Contains(path, wabaID):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":   wabaID,
				"name": "Discovered WABA",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer metaServer.Close()

	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	// Omit phone_id and waba_id
	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code": "test_code_only",
	})
	setEmbeddedSignupAuth(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

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
	assert.Equal(t, "v21.0", accountMap["api_version"])
}

func TestApp_ExchangeToken_CodeOnlyDiscoveryRejectsAmbiguousAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		wabaIDs        []string
		phoneIDs       []string
		wantMessage    string
		wantPhoneCalls int32
	}{
		{
			name:           "multiple WABAs",
			wabaIDs:        []string{"220000000000401", "220000000000402"},
			wantMessage:    "multiple WhatsApp Business Accounts",
			wantPhoneCalls: 0,
		},
		{
			name:           "multiple phone numbers",
			wabaIDs:        []string{"220000000000403"},
			phoneIDs:       []string{"110000000000403", "110000000000404"},
			wantMessage:    "multiple phone numbers",
			wantPhoneCalls: 1,
		},
		{
			name:           "missing WABA selection",
			wantMessage:    "did not provide a WhatsApp Business Account ID",
			wantPhoneCalls: 0,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := createAdminUser(t, app, org.ID)

			var pagesCalls int32
			var phoneCalls int32
			metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/oauth/access_token"):
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "ambiguous-discovery-token"})
				case strings.Contains(r.URL.Path, "/debug_token"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"app_id":                 embeddedSignupTestAppID,
							"is_valid":               true,
							"scopes":                 []string{"whatsapp_business_management", "whatsapp_business_messaging"},
							"expires_at":             time.Now().UTC().Add(2 * time.Hour).Unix(),
							"data_access_expires_at": time.Now().UTC().Add(time.Hour).Unix(),
							"granular_scopes": []map[string]any{
								{
									"scope":      "whatsapp_business_management",
									"target_ids": test.wabaIDs,
								},
							},
						},
					})
				case strings.Contains(r.URL.Path, "/me/accounts"):
					atomic.AddInt32(&pagesCalls, 1)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": []map[string]string{{"id": "facebook-page-not-waba"}},
					})
				case strings.HasSuffix(r.URL.Path, "/phone_numbers"):
					atomic.AddInt32(&phoneCalls, 1)
					phones := make([]map[string]string, 0, len(test.phoneIDs))
					for _, phoneID := range test.phoneIDs {
						phones = append(phones, map[string]string{
							"id":                   phoneID,
							"display_phone_number": "+1999999999",
							"verified_name":        "Ambiguous Phone",
						})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": phones})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer metaServer.Close()

			app.Config.WhatsApp.AppID = embeddedSignupTestAppID
			app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
			app.Config.WhatsApp.APIVersion = "v21.0"
			app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

			req := testutil.NewJSONRequest(t, map[string]any{"code": "ambiguous-code"})
			setEmbeddedSignupAuth(req, org.ID, user.ID)

			require.NoError(t, app.ExchangeToken(req))
			testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, test.wantMessage)
			assert.Equal(t, test.wantPhoneCalls, atomic.LoadInt32(&phoneCalls))
			assert.Zero(t, atomic.LoadInt32(&pagesCalls), "/me/accounts returns Facebook Pages and must not be used for WABA discovery")

			var accountCount int64
			require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ?", org.ID).
				Count(&accountCount).Error)
			assert.Zero(t, accountCount, "ambiguous discovery must not persist an account")
		})
	}
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
	setEmbeddedSignupAuth(req, org.ID, user.ID)

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
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
	setEmbeddedSignupAuth(req, org.ID, user.ID)

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
	org := testutil.CreateTestOrganization(t, app.DB)

	req := testutil.NewJSONRequest(t, map[string]interface{}{
		"code":     "test",
		"phone_id": "123",
		"waba_id":  "456",
	})
	req.RequestCtx.SetUserValue("organization_id", org.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	// No user identity is set, so the explicit organization can be resolved but
	// the accounts:write authorization still fails.

	err := app.ExchangeToken(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
}

func TestApp_ExchangeToken_RejectsUnpinnedOrganizationBeforeMeta(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	var metaCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		metaCalls.Add(1)
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	defer metaServer.Close()
	app.Config.WhatsApp.AppID = embeddedSignupTestAppID
	app.Config.WhatsApp.AppSecret = embeddedSignupTestAppSecret
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	for _, testCase := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "missing", wantStatus: fasthttp.StatusBadRequest},
		{name: "malformed", header: "not-a-workspace", wantStatus: fasthttp.StatusBadRequest},
		{name: "nonmember", header: otherOrg.ID.String(), wantStatus: fasthttp.StatusForbidden},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			req := testutil.NewJSONRequest(t, map[string]any{
				"code":     "must-not-be-exchanged",
				"phone_id": "must-not-be-read",
				"waba_id":  "must-not-be-read",
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			if testCase.header != "" {
				testutil.SetHeader(req, "X-Organization-ID", testCase.header)
			}
			require.NoError(t, app.ExchangeToken(req))
			assert.Equal(t, testCase.wantStatus, testutil.GetResponseStatusCode(req))
			assert.Zero(t, metaCalls.Load(), "organization rejection must happen before any Meta call")
		})
	}
}

// --- RegisterPhoneNumber Tests ---

func TestApp_RegisterPhoneNumber_ReusesDurablePINAndMovesToPendingSubscription(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	phoneID := fmt.Sprintf("123456789%d", time.Now().UnixNano()%1000000)
	accessToken, err := crypto.Encrypt("test_token", app.Config.App.EncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := crypto.Encrypt("654321", app.Config.App.EncryptionKey)
	require.NoError(t, err)

	// Embedded Signup committed both ciphertexts before the original provider
	// mutation. Recovery must reuse this exact candidate.
	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - RegisterPhone WithPIN",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    accessToken,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
		Pin:            pinCiphertext,
	}
	require.NoError(t, app.DB.Create(account).Error)

	// Mock Meta API server
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			// Registration call
			assert.Equal(t, "/v21.0/"+phoneID+"/register", r.URL.Path)
			assert.Equal(t, "Bearer test_token", r.Header.Get("Authorization"))

			var duringFlight models.WhatsAppAccount
			require.NoError(t, app.DB.Where("id = ?", account.ID).First(&duringFlight).Error)
			assert.Equal(t, "pending_registration", duringFlight.Status)
			assert.True(t, crypto.IsEncrypted(duringFlight.Pin))

			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.Equal(t, "654321", body["pin"])

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		} else {
			// GetPhoneNumberInfo call — return non-SMB phone info
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":            "123456789",
				"platform_type": "CLOUD_API",
			})
		}
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err = app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	err = json.Unmarshal(testutil.GetResponseBody(req), &resp)
	require.NoError(t, err)
	assert.True(t, resp.Data["success"].(bool))
	assert.Equal(t, "pending_subscription", resp.Data["status"])
	assert.NotContains(t, resp.Data, "pin")
	responseAccount, ok := resp.Data["account"].(map[string]interface{})
	require.True(t, ok)
	assert.NotContains(t, responseAccount, "pin")

	// Verify account status updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "pending_subscription", updated.Status)
	assert.True(t, crypto.IsEncrypted(updated.Pin))
	updated.DecryptSecrets(app.Config.App.EncryptionKey)
	assert.Equal(t, "654321", updated.Pin)

	// Verify audit log exists
	time.Sleep(50 * time.Millisecond)
	var auditCount int64
	require.NoError(t, app.DB.Model(&models.AuditLog{}).Where("organization_id = ? AND resource_type = ? AND action = ?", org.ID, "account", models.AuditActionUpdated).Count(&auditCount).Error)
	assert.Greater(t, auditCount, int64(0))
}

func TestApp_RegisterPhoneNumber_RejectsMissingDurablePINWithoutMetaCall(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	accessToken, err := crypto.Encrypt("test_token", app.Config.App.EncryptionKey)
	require.NoError(t, err)
	phoneID := fmt.Sprintf("223456789%d", time.Now().UnixNano()%1000000)

	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - GeneratedPIN",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    accessToken,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	var metaCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaCalls.Add(1)
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
				"id":            "123456789",
				"platform_type": "CLOUD_API",
			})
		}
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err = app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
	assert.Zero(t, metaCalls.Load())
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "pin")

	// Verify account status updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "pending_registration", updated.Status)
	assert.Empty(t, updated.Pin)
}

func TestApp_RegisterPhoneNumber_RegistrationFailed(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	accessToken, err := crypto.Encrypt("test_token", app.Config.App.EncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := crypto.Encrypt("123456", app.Config.App.EncryptionKey)
	require.NoError(t, err)
	phoneID := fmt.Sprintf("323456789%d", time.Now().UnixNano()%1000000)

	account := &models.WhatsAppAccount{
		OrganizationID: org.ID,
		Name:           "Test Account - RegFailed",
		PhoneID:        phoneID,
		BusinessID:     "987654321",
		AccessToken:    accessToken,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
		Pin:            pinCiphertext,
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
				Message: "Provider echoed secret 123456 while rejecting registration",
				Code:    368,
			},
		})
	}))
	defer metaServer.Close()

	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err = app.RegisterPhoneNumber(req)
	require.NoError(t, err)

	assert.Equal(t, fasthttp.StatusBadGateway, testutil.GetResponseStatusCode(req))
	body := string(testutil.GetResponseBody(req))
	assert.Contains(t, body, "remains pending registration")
	assert.NotContains(t, body, "123456")
	assert.NotContains(t, body, "Provider echoed")

	// Verify account status NOT updated
	var updated models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&updated).Error)
	assert.Equal(t, "pending_registration", updated.Status)
	assert.Equal(t, pinCiphertext, updated.Pin)
}

func TestApp_RegisterPhoneNumber_AccountNotFound(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
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

	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", "not-a-uuid")

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_RegisterPhoneNumber_RejectsCallerSelectedPINBeforeMeta(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createAdminUser(t, app, org.ID)
	var metaCalls atomic.Int32
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer metaServer.Close()
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)

	req := testutil.NewJSONRequest(t, map[string]interface{}{"pin": "999999"})
	setEmbeddedSignupAuth(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", uuid.New().String())

	require.NoError(t, app.RegisterPhoneNumber(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
	assert.Zero(t, metaCalls.Load())
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "999999")
}

func TestApp_RegisterPhoneNumber_CrossOrgIsolation(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)
	org1 := testutil.CreateTestOrganization(t, app.DB)
	org2 := testutil.CreateTestOrganization(t, app.DB)
	user2 := createAdminUser(t, app, org2.ID)

	// Create account in org1
	account := &models.WhatsAppAccount{
		OrganizationID: org1.ID,
		Name:           "Test Account - CrossOrg Isolation",
		PhoneID:        fmt.Sprintf("423456789%d", time.Now().UnixNano()%1000000),
		BusinessID:     "987654321",
		AccessToken:    "test_token",
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	// User from org2 tries to register org1's account
	req := testutil.NewJSONRequest(t, map[string]interface{}{})
	setEmbeddedSignupAuth(req, org2.ID, user2.ID)
	testutil.SetPathParam(req, "id", account.ID.String())

	err := app.RegisterPhoneNumber(req)
	require.NoError(t, err)
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}
