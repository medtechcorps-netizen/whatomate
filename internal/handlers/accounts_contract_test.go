package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

var contractGraphIDSequence atomic.Uint64

func contractGraphIDs() (string, string) {
	sequence := contractGraphIDSequence.Add(1)
	return fmt.Sprintf("110000000%06d", sequence), fmt.Sprintf("220000000%06d", sequence)
}

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
				meta.listedPhoneID = "999000000000001"
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
			const phoneID = "110000000000001"
			const wabaID = "220000000000001"
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			if testCase.configure != nil {
				testCase.configure(meta)
			}
			app := &App{
				Log:      testutil.NopLogger(),
				WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), meta.server.URL),
			}

			_, err := app.validateWhatsAppAccountContract(
				context.Background(),
				phoneID,
				wabaID,
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
	granularTargetIDs  []string
	messagingTargetIDs []string
	lookupStatus       int
	registrationStatus int
	subscriptionStatus int
	phoneIsOnBizApp    bool
	phonePlatformType  string
	tokensByCode       map[string]string
	hits               map[string]int
	onSubscribe        func()
	onRegister         func(map[string]string)
	onPhoneInfo        func(*http.Request)
}

func newWhatsAppContractMeta(t *testing.T, phoneID, wabaID string) *whatsappContractMeta {
	t.Helper()
	meta := &whatsappContractMeta{
		phoneID:            phoneID,
		wabaID:             wabaID,
		listedPhoneID:      phoneID,
		granularTargetIDs:  []string{wabaID},
		lookupStatus:       http.StatusOK,
		registrationStatus: http.StatusOK,
		subscriptionStatus: http.StatusOK,
		phonePlatformType:  "CLOUD_API",
		hits:               make(map[string]int),
	}
	meta.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta.mu.Lock()
		meta.hits[r.URL.Path]++
		meta.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/access_token"):
			if r.Method != http.MethodPost || r.URL.RawQuery != "" ||
				!strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
				http.Error(w, "invalid synthetic token exchange contract", http.StatusBadRequest)
				return
			}
			if err := r.ParseForm(); err != nil ||
				r.Form.Get("client_id") != "synthetic-meta-app" ||
				r.Form.Get("client_secret") != "synthetic-meta-app-secret" ||
				strings.TrimSpace(r.Form.Get("code")) == "" {
				http.Error(w, "invalid synthetic token exchange form", http.StatusBadRequest)
				return
			}
			token := "synthetic-embedded-token"
			if configured := meta.tokensByCode[r.Form.Get("code")]; configured != "" {
				token = configured
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
		case r.URL.Path == "/debug_token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"app_id":   "synthetic-meta-app",
					"is_valid": true,
					"scopes": []string{
						"whatsapp_business_management",
						"whatsapp_business_messaging",
					},
					"granular_scopes": []map[string]any{
						{
							"scope":      "whatsapp_business_management",
							"target_ids": meta.granularTargetIDs,
						},
						{
							"scope":      "whatsapp_business_messaging",
							"target_ids": meta.messagingTargetIDs,
						},
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
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if meta.onRegister != nil {
				meta.onRegister(body)
			}
			if meta.registrationStatus != http.StatusOK {
				w.WriteHeader(meta.registrationStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message":      "synthetic registration rejection",
						"code":         33,
						"is_transient": false,
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(r.URL.Path, "/"+meta.phoneID):
			if meta.onPhoneInfo != nil {
				meta.onPhoneInfo(r)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                       meta.phoneID,
				"display_phone_number":     "+60123456789",
				"verified_name":            "Synthetic Clinic",
				"code_verification_status": "VERIFIED",
				"account_mode":             "LIVE",
				"quality_rating":           "GREEN",
				"is_on_biz_app":            meta.phoneIsOnBizApp,
				"platform_type":            meta.phonePlatformType,
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

func (m *whatsappContractMeta) totalHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int
	for _, count := range m.hits {
		total += count
	}
	return total
}

func (m *whatsappContractMeta) pathHits(path string) int {
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
	phoneID, wabaID := contractGraphIDs()
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

func TestCreateAccountNormalizesRoutingIdentifiersBeforeValidationAndPersistence(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         "  Normalized Clinic  ",
		"phone_id":     "  " + phoneID + "  ",
		"business_id":  "  " + wabaID + "  ",
		"access_token": "synthetic-normalized-create-token",
		"api_version":  "  v21.0  ",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.CreateAccount(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.Equal(t, "Normalized Clinic", stored.Name)
	assert.Equal(t, phoneID, stored.PhoneID)
	assert.Equal(t, wabaID, stored.BusinessID)
	assert.Equal(t, "v21.0", stored.APIVersion)
	assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/phone_numbers"))
}

func TestCreateAccountRejectsPhoneWABAMismatchBeforePersistence(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.listedPhoneID = "999000000000002"
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
	phoneID, wabaID := contractGraphIDs()
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
	phoneID, wabaID := contractGraphIDs()
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

func TestUpdateAccountPendingRecoveryRejectsContractChangeWithoutMetaOrMutation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	accessToken, err := appcrypto.Encrypt("durable-pending-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := appcrypto.Encrypt("483920", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Pending Contract",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    accessToken,
		Pin:            pinCiphertext,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         "Must Not Replace Pending Claim",
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "replacement-token",
		"api_version":  "v21.0",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
	assert.Zero(t, meta.totalHits())

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, "Pending Contract", stored.Name)
	assert.Equal(t, accessToken, stored.AccessToken)
	assert.Equal(t, pinCiphertext, stored.Pin)
}

func TestUpdateAccountPendingRecoveryNonContractEditPreservesClaimCiphertexts(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	accessToken, err := appcrypto.Encrypt("durable-edit-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := appcrypto.Encrypt("739104", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Pending Editable Name",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    accessToken,
		Pin:            pinCiphertext,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":              "Safe Pending Display Edit",
		"auto_read_receipt": true,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Zero(t, meta.totalHits())

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, "Safe Pending Display Edit", stored.Name)
	assert.True(t, stored.AutoReadReceipt)
	assert.Equal(t, accessToken, stored.AccessToken)
	assert.Equal(t, pinCiphertext, stored.Pin)
}

func TestUpdateAccountActiveStalePUTCannotOverwriteEmbeddedSignupClaim(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.registrationStatus = http.StatusInternalServerError
	meta.tokensByCode = map[string]string{
		"claim-during-put": "synthetic-exchange-claim-token",
	}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	oldToken, err := appcrypto.Encrypt("active-token-before-put", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("190275", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Active Before Stale PUT",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		Pin:            oldPIN,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(account).Error)

	validationEntered := make(chan struct{})
	releaseValidation := make(chan struct{})
	meta.onPhoneInfo = func(r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-put-token" {
			return
		}
		close(validationEntered)
		<-releaseValidation
	}
	t.Cleanup(func() {
		select {
		case <-releaseValidation:
		default:
			close(releaseValidation)
		}
	})

	updateReq := testutil.NewJSONRequest(t, map[string]any{
		"name":         "Stale PUT Must Not Win",
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "synthetic-put-token",
		"api_version":  "v21.0",
	})
	testutil.SetAuthContext(updateReq, org.ID, user.ID)
	testutil.SetPathParam(updateReq, "id", account.ID.String())
	updateDone := make(chan error, 1)
	go func() { updateDone <- app.UpdateAccount(updateReq) }()

	select {
	case <-validationEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("active PUT did not pause during provider validation")
	}

	exchangeReq := testutil.NewJSONRequest(t, map[string]any{
		"code":     "claim-during-put",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(exchangeReq, org.ID, user.ID)
	testutil.SetHeader(exchangeReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(exchangeReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exchangeReq))

	var claimed models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&claimed).Error)
	require.Equal(t, "pending_registration", claimed.Status)
	require.NotEqual(t, oldToken, claimed.AccessToken)
	require.NotEqual(t, oldPIN, claimed.Pin)
	claimTokenCiphertext := claimed.AccessToken
	claimPINCiphertext := claimed.Pin

	close(releaseValidation)
	require.NoError(t, <-updateDone)
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(updateReq))

	var final models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&final).Error)
	assert.Equal(t, "pending_registration", final.Status)
	assert.Equal(t, "Active Before Stale PUT", final.Name)
	assert.Equal(t, claimTokenCiphertext, final.AccessToken)
	assert.Equal(t, claimPINCiphertext, final.Pin)
	assert.Zero(t, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestRegisterRecoveryConcurrentContractPUTCannotSkipToActive(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := integrationTestUser(t, app, org.ID, "accounts:read", "accounts:write", "accounts:delete")

	accessToken, err := appcrypto.Encrypt("durable-race-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := appcrypto.Encrypt("640281", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Pending Race Contract",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    accessToken,
		Pin:            pinCiphertext,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	registerEntered := make(chan struct{})
	releaseRegister := make(chan struct{})
	meta.onRegister = func(body map[string]string) {
		assert.Equal(t, "640281", body["pin"])
		close(registerEntered)
		<-releaseRegister
	}
	t.Cleanup(func() {
		select {
		case <-releaseRegister:
		default:
			close(releaseRegister)
		}
	})

	registerReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(registerReq, org.ID, user.ID)
	testutil.SetHeader(registerReq, "X-Organization-ID", org.ID.String())
	testutil.SetPathParam(registerReq, "id", account.ID.String())
	registerDone := make(chan error, 1)
	go func() { registerDone <- app.RegisterPhoneNumber(registerReq) }()

	select {
	case <-registerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration recovery did not reach Meta")
	}

	deleteReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(deleteReq, org.ID, user.ID)
	testutil.SetPathParam(deleteReq, "id", account.ID.String())
	require.NoError(t, app.DeleteAccount(deleteReq))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(deleteReq))

	var duringRegistration models.WhatsAppAccount
	require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&duringRegistration).Error)
	assert.False(t, duringRegistration.DeletedAt.Valid)
	assert.Equal(t, "pending_registration", duringRegistration.Status)
	assert.Equal(t, accessToken, duringRegistration.AccessToken)
	assert.Equal(t, pinCiphertext, duringRegistration.Pin)

	updateReq := testutil.NewJSONRequest(t, map[string]any{
		"name":         "Must Not Race Registration",
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "replacement-race-token",
		"api_version":  "v21.0",
	})
	testutil.SetAuthContext(updateReq, org.ID, user.ID)
	testutil.SetPathParam(updateReq, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(updateReq))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(updateReq))

	close(releaseRegister)
	require.NoError(t, <-registerDone)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(registerReq))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "pending_subscription", stored.Status)
	assert.Equal(t, "Pending Race Contract", stored.Name)
	assert.Equal(t, accessToken, stored.AccessToken)
	assert.Equal(t, pinCiphertext, stored.Pin)
	assert.Equal(t, 1, meta.pathHits("/v21.0/"+phoneID+"/register"))
	assert.Zero(t, meta.pathHits("/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestSubscribeRecoveryRejectsPendingRegistrationBeforeMeta(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	accessToken, err := appcrypto.Encrypt("pending-register-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pinCiphertext, err := appcrypto.Encrypt("512804", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Registration Must Finish First",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    accessToken,
		Pin:            pinCiphertext,
		APIVersion:     "v21.0",
		Status:         "pending_registration",
	}
	require.NoError(t, app.DB.Create(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.SubscribeApp(req))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
	assert.Zero(t, meta.totalHits())

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, accessToken, stored.AccessToken)
	assert.Equal(t, pinCiphertext, stored.Pin)
}

func TestSubscribeRecoveryCASDoesNotClobberNewerClaim(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	originalToken, err := appcrypto.Encrypt("subscription-token-a", integrationTestEncryptionKey)
	require.NoError(t, err)
	newerToken, err := appcrypto.Encrypt("subscription-token-b", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Subscription CAS Clinic",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    originalToken,
		APIVersion:     "v21.0",
		Status:         "pending_subscription",
	}
	require.NoError(t, app.DB.Create(account).Error)

	subscribeEntered := make(chan struct{})
	releaseSubscribe := make(chan struct{})
	meta.onSubscribe = func() {
		close(subscribeEntered)
		<-releaseSubscribe
	}
	t.Cleanup(func() {
		select {
		case <-releaseSubscribe:
		default:
			close(releaseSubscribe)
		}
	})

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	testutil.SetPathParam(req, "id", account.ID.String())
	done := make(chan error, 1)
	go func() { done <- app.SubscribeApp(req) }()

	select {
	case <-subscribeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription recovery did not reach Meta")
	}
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Updates(map[string]any{
			"access_token": newerToken,
			"status":       "pending_registration",
		}).Error)
	close(releaseSubscribe)
	require.NoError(t, <-done)
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, newerToken, stored.AccessToken)
	assert.Equal(t, 1, meta.pathHits("/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestManualAccountSubscribeStatusFenceDoesNotClobberRevivedEmbeddedSignupClaim(t *testing.T) {
	for _, flow := range []string{"create", "update"} {
		t.Run(flow, func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			require.False(t, app.Config.Database.RLSEnabled, "this regression covers the non-RLS handler path")
			org := testutil.CreateTestOrganization(t, app.DB)
			user := integrationTestUser(t, app, org.ID, "accounts:read", "accounts:write", "accounts:delete")

			subscribeEntered := make(chan struct{})
			releaseSubscribe := make(chan struct{})
			meta.onSubscribe = func() {
				close(subscribeEntered)
				<-releaseSubscribe
			}
			t.Cleanup(func() {
				select {
				case <-releaseSubscribe:
				default:
					close(releaseSubscribe)
				}
			})

			var req *fastglue.Request
			switch flow {
			case "create":
				req = testutil.NewJSONRequest(t, map[string]any{
					"name":         "Manual Create Clinic",
					"phone_id":     phoneID,
					"business_id":  wabaID,
					"access_token": "synthetic-manual-create-token",
					"api_version":  "v21.0",
				})
				testutil.SetAuthContext(req, org.ID, user.ID)
			case "update":
				oldToken, err := appcrypto.Encrypt("synthetic-manual-old-token", integrationTestEncryptionKey)
				require.NoError(t, err)
				account := &models.WhatsAppAccount{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: org.ID,
					Name:           "Manual Update Clinic",
					PhoneID:        "330000000000001",
					BusinessID:     "440000000000001",
					AccessToken:    oldToken,
					APIVersion:     "v21.0",
					Status:         "active",
				}
				require.NoError(t, app.DB.Create(account).Error)
				req = testutil.NewJSONRequest(t, map[string]any{
					"name":         "Manual Update Clinic",
					"phone_id":     phoneID,
					"business_id":  wabaID,
					"access_token": "synthetic-manual-update-token",
					"api_version":  "v21.0",
				})
				testutil.SetAuthContext(req, org.ID, user.ID)
				testutil.SetPathParam(req, "id", account.ID.String())
			default:
				t.Fatalf("unsupported flow %q", flow)
			}

			done := make(chan error, 1)
			go func() {
				if flow == "create" {
					done <- app.CreateAccount(req)
					return
				}
				done <- app.UpdateAccount(req)
			}()

			select {
			case <-subscribeEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("manual account flow did not reach Meta Subscribe")
			}

			var pending models.WhatsAppAccount
			require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&pending).Error)
			require.Equal(t, "pending_subscription", pending.Status)
			require.True(t, appcrypto.IsEncrypted(pending.AccessToken))
			oldTokenCiphertext := pending.AccessToken

			deleteReq := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(deleteReq, org.ID, user.ID)
			testutil.SetPathParam(deleteReq, "id", pending.ID.String())
			require.NoError(t, app.DeleteAccount(deleteReq))
			require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(deleteReq))
			var guarded models.WhatsAppAccount
			require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", pending.ID, org.ID).First(&guarded).Error)
			assert.False(t, guarded.DeletedAt.Valid)
			assert.Equal(t, "pending_subscription", guarded.Status)
			assert.Equal(t, oldTokenCiphertext, guarded.AccessToken)

			newerTokenCiphertext, err := appcrypto.Encrypt("synthetic-revived-claim-token-"+flow, integrationTestEncryptionKey)
			require.NoError(t, err)
			newerPINCiphertext, err := appcrypto.Encrypt("731904", integrationTestEncryptionKey)
			require.NoError(t, err)
			require.NotEqual(t, oldTokenCiphertext, newerTokenCiphertext)
			require.NoError(t, app.DB.Delete(&pending).Error)
			revive := app.DB.Unscoped().Model(&models.WhatsAppAccount{}).
				Where("id = ? AND organization_id = ?", pending.ID, org.ID).
				Updates(map[string]any{
					"deleted_at":   nil,
					"access_token": newerTokenCiphertext,
					"pin":          newerPINCiphertext,
					"status":       "pending_registration",
				})
			require.NoError(t, revive.Error)
			require.Equal(t, int64(1), revive.RowsAffected)

			close(releaseSubscribe)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("manual account flow did not finish after Subscribe was released")
			}
			require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))

			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", pending.ID, org.ID).First(&stored).Error)
			assert.False(t, stored.DeletedAt.Valid)
			assert.Equal(t, "pending_registration", stored.Status)
			assert.Equal(t, newerTokenCiphertext, stored.AccessToken)
			assert.Equal(t, newerPINCiphertext, stored.Pin)
			assert.Zero(t, meta.pathHits("/v21.0/"+phoneID+"/register"))
			assert.Equal(t, 1, meta.pathHits("/v21.0/"+wabaID+"/subscribed_apps"))
		})
	}
}

func TestUpdateAccountContractChangeValidatesThenResubscribes(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
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
		"name":         "  Normalized Updated Contract  ",
		"phone_id":     "  " + phoneID + "  ",
		"business_id":  "  " + wabaID + "  ",
		"access_token": "synthetic-updated-token",
		"api_version":  "  v21.0  ",
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
	assert.Equal(t, "Normalized Updated Contract", stored.Name)
	assert.Equal(t, "v21.0", stored.APIVersion)
	assert.True(t, appcrypto.IsEncrypted(stored.AccessToken))
}

func TestEmbeddedSignupSuppliedIDsCannotBypassRelationshipValidation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.listedPhoneID = "999000000000003"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-authorization-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
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

func TestEmbeddedSignupSuppliedWABAMustBeGrantedByToken(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.granularTargetIDs = []string{"999000000000004"}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-ungranted-waba-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "not granted")
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/phone_numbers"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestEmbeddedSignupSuppliedWABAMustMatchMessagingGranularTarget(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.messagingTargetIDs = []string{"999000000000005"}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-ungranted-messaging-waba-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "not granted for messaging")
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/phone_numbers"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestEmbeddedSignupRegisterSuccessRetainsDurablePINWhenClaimIsSuperseded(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	var observedPIN string
	var callbackErr error
	meta.onRegister = func(body map[string]string) {
		observedPIN = body["pin"]
		var claimed models.WhatsAppAccount
		if err := app.DB.Where("organization_id = ? AND BTRIM(phone_id) = BTRIM(?)", org.ID, phoneID).
			First(&claimed).Error; err != nil {
			callbackErr = err
			return
		}
		if claimed.Status != "pending_registration" ||
			!appcrypto.IsEncrypted(claimed.AccessToken) ||
			!appcrypto.IsEncrypted(claimed.Pin) {
			callbackErr = errors.New("register did not observe a durable encrypted pending claim")
			return
		}
		decrypted := claimed
		decrypted.DecryptSecrets(integrationTestEncryptionKey)
		if len(observedPIN) != 6 || decrypted.Pin != observedPIN {
			callbackErr = errors.New("registered PIN did not match the durable claim")
			return
		}
		supersedingToken, err := appcrypto.Encrypt("synthetic-newer-claim-token", integrationTestEncryptionKey)
		if err != nil {
			callbackErr = err
			return
		}
		callbackErr = app.DB.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ?", claimed.ID, org.ID).
			Update("access_token", supersedingToken).Error
	}

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-durable-pin-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.NoError(t, callbackErr)
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "replaced by a newer request")
	assert.Equal(t, 1, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.True(t, appcrypto.IsEncrypted(stored.Pin))
	require.Len(t, observedPIN, 6)
	decrypted := stored
	decrypted.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, observedPIN, decrypted.Pin, "a successful external registration must retain its recoverable PIN")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), observedPIN)
}

func TestEmbeddedSignupConcurrentClaimCannotOverwriteInFlightPINOrToken(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.tokensByCode = map[string]string{
		"first-inflight-code":  "synthetic-first-inflight-token",
		"second-inflight-code": "synthetic-second-inflight-token",
	}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	firstPIN := make(chan string, 1)
	releaseFirst := make(chan struct{})
	var registerCalls atomic.Int32
	meta.onRegister = func(body map[string]string) {
		if registerCalls.Add(1) == 1 {
			firstPIN <- body["pin"]
			<-releaseFirst
		}
	}
	firstReq := testutil.NewJSONRequest(t, map[string]any{
		"code":     "first-inflight-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(firstReq, org.ID, user.ID)
	testutil.SetHeader(firstReq, "X-Organization-ID", org.ID.String())
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- app.ExchangeToken(firstReq)
	}()

	var acceptedCandidate string
	select {
	case acceptedCandidate = <-firstPIN:
	case <-time.After(2 * time.Second):
		close(releaseFirst)
		t.Fatal("first registration did not reach Meta")
	}
	require.Len(t, acceptedCandidate, 6)

	secondReq := testutil.NewJSONRequest(t, map[string]any{
		"code":     "second-inflight-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(secondReq, org.ID, user.ID)
	testutil.SetHeader(secondReq, "X-Organization-ID", org.ID.String())
	secondErr := app.ExchangeToken(secondReq)
	secondStatus := testutil.GetResponseStatusCode(secondReq)
	secondBody := string(testutil.GetResponseBody(secondReq))

	var duringFlight models.WhatsAppAccount
	duringFlightErr := app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&duringFlight).Error
	if duringFlightErr == nil {
		duringFlight.DecryptSecrets(integrationTestEncryptionKey)
	}
	close(releaseFirst)
	firstErr := <-firstDone

	require.NoError(t, secondErr)
	require.NoError(t, firstErr)
	assert.Equal(t, fasthttp.StatusConflict, secondStatus)
	assert.Contains(t, secondBody, "pending reconciliation")
	require.NoError(t, duringFlightErr)
	assert.Equal(t, "pending_registration", duringFlight.Status)
	assert.Equal(t, "synthetic-first-inflight-token", duringFlight.AccessToken)
	assert.Equal(t, acceptedCandidate, duringFlight.Pin)
	assert.Equal(t, int32(1), registerCalls.Load(), "the rejected claim must not call Meta register")

	var final models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&final).Error)
	final.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, "active", final.Status)
	assert.Equal(t, "synthetic-first-inflight-token", final.AccessToken)
	assert.Equal(t, acceptedCandidate, final.Pin)
}

func TestEmbeddedSignupReusesAuthoritativeSMBValidationWithoutRegistration(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-authoritative-smb-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, 1, meta.hit("/v21.0/"+phoneID), "embedded signup must reuse the authoritative validation read")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.True(t, stored.IsSMB)
	assert.Empty(t, stored.Pin)
	assert.Equal(t, "active", stored.Status)
}

func TestEmbeddedSignupActiveReconnectPreservesStableNameAndOperationalFlags(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	var observedRegistrationPIN string
	meta.onRegister = func(body map[string]string) {
		observedRegistrationPIN = body["pin"]
	}
	oldToken, err := appcrypto.Encrypt("synthetic-old-reconnect-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("123456", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         org.ID,
		Name:                   "Stable Clinic Routing Name",
		PhoneID:                "  " + phoneID + "  ",
		BusinessID:             "old-business-id",
		AccessToken:            oldToken,
		APIVersion:             "v20.0",
		Status:                 "active",
		Pin:                    oldPIN,
		IsDefaultIncoming:      true,
		IsDefaultOutgoing:      true,
		AutoReadReceipt:        true,
		BusinessCallingEnabled: true,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-active-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
		"name":     "Must Not Replace Stable Name",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", existing.ID, org.ID).First(&stored).Error)
	assert.Equal(t, existing.ID, stored.ID)
	assert.Equal(t, "Stable Clinic Routing Name", stored.Name)
	assert.Equal(t, phoneID, stored.PhoneID)
	assert.True(t, stored.IsDefaultIncoming)
	assert.True(t, stored.IsDefaultOutgoing)
	assert.True(t, stored.AutoReadReceipt)
	assert.True(t, stored.BusinessCallingEnabled)
	assert.Equal(t, "active", stored.Status)
	require.Len(t, observedRegistrationPIN, 6)
	decrypted := stored
	decrypted.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, observedRegistrationPIN, decrypted.Pin, "successful registration must retain the accepted candidate PIN")
}

func TestEmbeddedSignupActiveReconnectDefiniteRejectionRestoresPriorStatusAndPIN(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.registrationStatus = http.StatusBadRequest
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-rejected-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("654321", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         org.ID,
		Name:                   "Stable Rejected Clinic",
		PhoneID:                phoneID,
		BusinessID:             "old-rejected-waba",
		AccessToken:            oldToken,
		APIVersion:             "v21.0",
		Status:                 "active",
		Pin:                    oldPIN,
		IsDefaultIncoming:      true,
		IsDefaultOutgoing:      true,
		AutoReadReceipt:        true,
		BusinessCallingEnabled: true,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-rejected-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
		"name":     "Do Not Rename Rejected Clinic",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Contains(t, string(testutil.GetResponseBody(req)), "Registration could not be confirmed")
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", existing.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, "Stable Rejected Clinic", stored.Name)
	assert.True(t, stored.IsDefaultIncoming)
	assert.True(t, stored.IsDefaultOutgoing)
	assert.True(t, stored.AutoReadReceipt)
	assert.True(t, stored.BusinessCallingEnabled)
	assert.True(t, appcrypto.IsEncrypted(stored.AccessToken))
	assert.True(t, appcrypto.IsEncrypted(stored.Pin))
	decrypted := stored
	decrypted.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, "synthetic-embedded-token", decrypted.AccessToken)
	assert.Equal(t, "654321", decrypted.Pin, "a definite rejection must restore the last PIN Meta accepted")
}

func TestEmbeddedSignupActiveReconnectTimeoutKeepsCandidatePending(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	pinObserved := make(chan string, 1)
	meta.onRegister = func(body map[string]string) {
		pinObserved <- body["pin"]
		time.Sleep(300 * time.Millisecond)
	}
	app := newWhatsAppContractApp(t, meta)
	app.WhatsApp.HTTPClient.Timeout = 100 * time.Millisecond
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-timeout-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("222222", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Stable Timeout Clinic",
		PhoneID:        phoneID,
		BusinessID:     "old-timeout-waba",
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		Pin:            oldPIN,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-timeout-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var candidatePIN string
	select {
	case candidatePIN = <-pinObserved:
	case <-time.After(time.Second):
		t.Fatal("registration request did not carry the candidate PIN")
	}
	require.Len(t, candidatePIN, 6)
	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", existing.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status, "an ambiguous timeout must remain pending reconciliation")
	assert.Equal(t, "Stable Timeout Clinic", stored.Name)
	decrypted := stored
	decrypted.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, "synthetic-embedded-token", decrypted.AccessToken)
	assert.Equal(t, candidatePIN, decrypted.Pin, "the possibly accepted candidate PIN must remain recoverable")
}

func TestEmbeddedSignupSoftDeletedReconnectRestoresAndNormalizesAccount(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-deleted-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	deleted := &models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         org.ID,
		Name:                   "Deleted Clinic Name",
		PhoneID:                "  " + phoneID + "  ",
		BusinessID:             "deleted-business-id",
		AccessToken:            oldToken,
		APIVersion:             "v20.0",
		Status:                 "inactive",
		IsDefaultIncoming:      true,
		IsDefaultOutgoing:      true,
		AutoReadReceipt:        true,
		BusinessCallingEnabled: true,
	}
	require.NoError(t, app.DB.Create(deleted).Error)
	require.NoError(t, app.DB.Delete(deleted).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-soft-delete-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
		"name":     "Restored Clinic Name",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", deleted.ID, org.ID).First(&stored).Error)
	assert.Equal(t, deleted.ID, stored.ID)
	assert.False(t, stored.DeletedAt.Valid, "reconnect must explicitly reclaim the soft-deleted row")
	assert.Equal(t, phoneID, stored.PhoneID)
	assert.Equal(t, "Deleted Clinic Name", stored.Name, "restoring the same row must preserve name-keyed historical relationships")
	assert.False(t, stored.IsDefaultIncoming)
	assert.False(t, stored.IsDefaultOutgoing)
	assert.False(t, stored.AutoReadReceipt)
	assert.False(t, stored.BusinessCallingEnabled)
	assert.Equal(t, "active", stored.Status)
}

func TestEmbeddedSignupGlobalPhoneConflictRejectsBeforeProviderMutation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	ownerOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, targetOrg.ID)
	ownerToken, err := appcrypto.Encrypt("synthetic-owner-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	owner := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: ownerOrg.ID,
		Name:           "Existing Global Owner",
		PhoneID:        "  " + phoneID + "  ",
		BusinessID:     "existing-owner-waba",
		AccessToken:    ownerToken,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(owner).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-global-conflict-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, targetOrg.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", targetOrg.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), ownerOrg.ID.String())
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var targetCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND BTRIM(phone_id) = BTRIM(?)", targetOrg.ID, phoneID).
		Count(&targetCount).Error)
	assert.Zero(t, targetCount)
	var unchanged models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", owner.ID).First(&unchanged).Error)
	assert.Equal(t, ownerOrg.ID, unchanged.OrganizationID)
	assert.Equal(t, "active", unchanged.Status)
	assert.Equal(t, "  "+phoneID+"  ", unchanged.PhoneID)
}

func TestEmbeddedSignupExplicitAlternateMembershipWritesOnlySelectedOrganization(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	homeRole := testutil.CreateTestRoleWithKeys(t, app.DB, homeOrg.ID, "embedded-home-role", []string{"accounts:write"})
	user := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithRoleID(&homeRole.ID))
	targetRole := testutil.CreateTestRoleWithKeys(
		t,
		app.DB,
		targetOrg.ID,
		"embedded-target-role",
		[]string{"accounts:read", "accounts:write"},
	)
	require.NoError(t, app.DB.Create(&models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         user.ID,
		OrganizationID: targetOrg.ID,
		RoleID:         &targetRole.ID,
		IsDefault:      false,
	}).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-alternate-member-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, homeOrg.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", targetOrg.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var targetCount, homeCount int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", targetOrg.ID, phoneID).
		Count(&targetCount).Error)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", homeOrg.ID, phoneID).
		Count(&homeCount).Error)
	assert.Equal(t, int64(1), targetCount)
	assert.Zero(t, homeCount)
}

func TestEmbeddedSignupSuperAdminCanPinExistingTargetOrganization(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithSuperAdmin())

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-superadmin-target-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, homeOrg.ID, superAdmin.ID)
	testutil.SetHeader(req, "X-Organization-ID", targetOrg.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", targetOrg.ID, phoneID).First(&stored).Error)
	assert.Equal(t, targetOrg.ID, stored.OrganizationID)
	assert.Equal(t, "active", stored.Status)
}

func TestEmbeddedSignupDeletedExplicitTargetFailsBeforeMeta(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		configureUser func(*testing.T, *App, uuid.UUID, uuid.UUID) *models.User
	}{
		{
			name: "member with stale target membership",
			configureUser: func(t *testing.T, app *App, homeOrgID, targetOrgID uuid.UUID) *models.User {
				homeRole := testutil.CreateTestRoleWithKeys(t, app.DB, homeOrgID, "deleted-home-role", []string{"accounts:write"})
				user := testutil.CreateTestUser(t, app.DB, homeOrgID, testutil.WithRoleID(&homeRole.ID))
				targetRole := testutil.CreateTestRoleWithKeys(t, app.DB, targetOrgID, "deleted-target-role", []string{"accounts:write"})
				require.NoError(t, app.DB.Create(&models.UserOrganization{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					UserID:         user.ID,
					OrganizationID: targetOrgID,
					RoleID:         &targetRole.ID,
				}).Error)
				return user
			},
		},
		{
			name: "superadmin target",
			configureUser: func(t *testing.T, app *App, homeOrgID, _ uuid.UUID) *models.User {
				return testutil.CreateTestUser(t, app.DB, homeOrgID, testutil.WithSuperAdmin())
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			homeOrg := testutil.CreateTestOrganization(t, app.DB)
			targetOrg := testutil.CreateTestOrganization(t, app.DB)
			user := testCase.configureUser(t, app, homeOrg.ID, targetOrg.ID)
			require.NoError(t, app.DB.Delete(targetOrg).Error)

			req := testutil.NewJSONRequest(t, map[string]any{
				"code":     "must-not-exchange-deleted-target",
				"phone_id": phoneID,
				"waba_id":  wabaID,
			})
			testutil.SetAuthContext(req, homeOrg.ID, user.ID)
			testutil.SetHeader(req, "X-Organization-ID", targetOrg.ID.String())
			require.NoError(t, app.ExchangeToken(req))
			testutil.AssertErrorResponse(t, req, fasthttp.StatusForbidden, "not available")
			assert.Zero(t, meta.totalHits(), "deleted workspaces must be rejected before any Meta call")
		})
	}
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
