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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var contractGraphIDSequence atomic.Uint64

const (
	contractMetaAppID     = "990000000000001"
	contractMetaAppSecret = "synthetic-meta-app-secret"
)

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
	permanentToken     bool
	subscribedAppIDs   []string
	tokensByCode       map[string]string
	hits               map[string]int
	onSubscribe        func()
	onSubscriptionRead func()
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
		subscribedAppIDs:   []string{contractMetaAppID},
		hits:               make(map[string]int),
	}
	meta.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta.mu.Lock()
		meta.hits[r.URL.Path]++
		meta.hits[r.Method+" "+r.URL.Path]++
		meta.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/access_token"):
			if r.Method != http.MethodPost || r.URL.RawQuery != "" ||
				!strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
				http.Error(w, "invalid synthetic token exchange contract", http.StatusBadRequest)
				return
			}
			if err := r.ParseForm(); err != nil ||
				r.Form.Get("client_id") != contractMetaAppID ||
				r.Form.Get("client_secret") != contractMetaAppSecret ||
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
			debugData := map[string]any{
				"app_id":   contractMetaAppID,
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
			}
			if !meta.permanentToken {
				debugData["expires_at"] = time.Now().UTC().Add(2 * time.Hour).Unix()
				debugData["data_access_expires_at"] = time.Now().UTC().Add(time.Hour).Unix()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": debugData,
			})
		case strings.HasSuffix(r.URL.Path, "/"+meta.wabaID+"/subscribed_apps"):
			if r.Method == http.MethodGet {
				if meta.onSubscriptionRead != nil {
					meta.onSubscriptionRead()
				}
				data := make([]map[string]any, 0, len(meta.subscribedAppIDs))
				for _, appID := range meta.subscribedAppIDs {
					data = append(data, map[string]any{
						"whatsapp_business_api_data": map[string]string{"id": appID},
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "invalid synthetic subscription method", http.StatusMethodNotAllowed)
				return
			}
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
	for key, count := range m.hits {
		if strings.Contains(key, " ") {
			continue
		}
		total += count
	}
	return total
}

func (m *whatsappContractMeta) pathHits(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

func (m *whatsappContractMeta) methodHits(method, path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[method+" "+path]
}

func newWhatsAppContractApp(t *testing.T, meta *whatsappContractMeta) *App {
	t.Helper()
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.WhatsApp.AppID = contractMetaAppID
	app.Config.WhatsApp.AppSecret = contractMetaAppSecret
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
	assert.Zero(t, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 1, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))

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

func TestUpdateAccountActiveStalePUTCannotOverwriteEmbeddedSignupTokenRefresh(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
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
	require.Equal(t, "active", claimed.Status)
	require.NotEqual(t, oldToken, claimed.AccessToken)
	require.Equal(t, oldPIN, claimed.Pin)
	claimTokenCiphertext := claimed.AccessToken

	close(releaseValidation)
	require.NoError(t, <-updateDone)
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(updateReq))

	var final models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&final).Error)
	assert.Equal(t, "active", final.Status)
	assert.Equal(t, "Active Before Stale PUT", final.Name)
	assert.Equal(t, claimTokenCiphertext, final.AccessToken)
	assert.Equal(t, oldPIN, final.Pin)
	assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
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
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-reconnect-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("123456", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldExpiry := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	existing := &models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         org.ID,
		Name:                   "Stable Clinic Routing Name",
		PhoneID:                phoneID,
		BusinessID:             wabaID,
		AccessToken:            oldToken,
		AccessTokenExpiresAt:   &oldExpiry,
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
	assert.Equal(t, oldPIN, stored.Pin, "token refresh must preserve the accepted PIN ciphertext")
	assert.Nil(t, stored.AccessTokenExpiresAt, "a permanent replacement token must remove the old expiry")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 0, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
	decrypted := stored
	decrypted.DecryptSecrets(integrationTestEncryptionKey)
	assert.Equal(t, "synthetic-embedded-token", decrypted.AccessToken)
	assert.Equal(t, "123456", decrypted.Pin)
}

func TestEmbeddedSignupActiveReconnectFailsClosedWhenCurrentAppIsNotSubscribed(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	meta.subscribedAppIDs = []string{"990000000000002"}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-unbound-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("729164", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Name:              "Stable Unbound Clinic",
		PhoneID:           phoneID,
		BusinessID:        wabaID,
		AccessToken:       oldToken,
		APIVersion:        "v21.0",
		Status:            "active",
		Pin:               oldPIN,
		AutoReadReceipt:   true,
		IsDefaultIncoming: true,
		IsDefaultOutgoing: true,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-unbound-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "not subscribed")

	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", existing.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.Equal(t, oldPIN, stored.Pin)
	assert.Equal(t, existing.Name, stored.Name)
	assert.True(t, stored.AutoReadReceipt)
	assert.True(t, stored.IsDefaultIncoming)
	assert.True(t, stored.IsDefaultOutgoing)
}

func TestEmbeddedSignupActiveReconnectFencesConcurrentIntegrationCenterChange(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-config-race-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("284517", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Stable Config Race Clinic",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		Pin:            oldPIN,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	subscriptionReadEntered := make(chan struct{})
	allowSubscriptionRead := make(chan struct{})
	meta.onSubscriptionRead = func() {
		close(subscriptionReadEntered)
		<-allowSubscriptionRead
	}
	configLocked := make(chan struct{})
	releaseConfig := make(chan struct{})
	t.Cleanup(func() {
		for _, ch := range []chan struct{}{allowSubscriptionRead, releaseConfig} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":     "synthetic-config-race-reconnect-code",
		"phone_id": phoneID,
		"waba_id":  wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	exchangeDone := make(chan error, 1)
	go func() { exchangeDone <- app.ExchangeToken(req) }()
	select {
	case <-subscriptionReadEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("active reconnect did not reach subscription proof")
	}

	configDone := make(chan error, 1)
	go func() {
		configDone <- app.DB.Transaction(func(tx *gorm.DB) error {
			var lockedOrganization models.Organization
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ?", org.ID).
				First(&lockedOrganization).Error; err != nil {
				return err
			}
			var integration models.ProviderIntegration
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("organization_id = ? AND provider = ?", org.ID, integrationProviderMeta).
				First(&integration).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if lockedOrganization.Settings == nil {
				lockedOrganization.Settings = models.JSONB{}
			}
			lockedOrganization.Settings["meta_app_id"] = "990000000000099"
			if err := tx.Model(&models.Organization{}).
				Where("id = ?", org.ID).
				Update("settings", lockedOrganization.Settings).Error; err != nil {
				return err
			}
			close(configLocked)
			<-releaseConfig
			return nil
		})
	}()
	select {
	case <-configLocked:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Integration Center update did not acquire its contract locks")
	}
	close(allowSubscriptionRead)
	select {
	case err := <-exchangeDone:
		t.Fatalf("reconnect bypassed the Integration Center lock fence: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseConfig)
	require.NoError(t, <-configDone)
	require.NoError(t, <-exchangeDone)
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "settings changed")

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", existing.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.Equal(t, oldPIN, stored.Pin)
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestEmbeddedSignupActiveReconnectDifferentWABARejectsBeforeMutation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
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
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", existing.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, "Stable Rejected Clinic", stored.Name)
	assert.True(t, stored.IsDefaultIncoming)
	assert.True(t, stored.IsDefaultOutgoing)
	assert.True(t, stored.AutoReadReceipt)
	assert.True(t, stored.BusinessCallingEnabled)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.Equal(t, oldPIN, stored.Pin)
}

func TestEmbeddedSignupActiveReconnectDifferentAPIRejectsBeforeMutation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
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
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v20.0",
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
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))
	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", existing.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, "Stable Timeout Clinic", stored.Name)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.Equal(t, oldPIN, stored.Pin)
}

func createRegistrationReconciliationFixture(
	t *testing.T,
	app *App,
	orgID uuid.UUID,
	phoneID, wabaID string,
	pendingSince time.Time,
	withEvidence bool,
) (*models.WhatsAppAccount, *models.Message) {
	t.Helper()
	accessToken, err := appcrypto.Encrypt("synthetic-permanent-pending-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pin, err := appcrypto.Encrypt("820641", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: pendingSince.Add(-time.Hour),
			UpdatedAt: pendingSince,
		},
		OrganizationID:         orgID,
		Name:                   "Pending Reconciliation Clinic " + phoneID,
		PhoneID:                phoneID,
		BusinessID:             wabaID,
		AccessToken:            accessToken,
		APIVersion:             "v21.0",
		Status:                 "pending_registration",
		Pin:                    pin,
		IsDefaultIncoming:      true,
		IsDefaultOutgoing:      true,
		AutoReadReceipt:        true,
		BusinessCallingEnabled: true,
	}
	require.NoError(t, app.DB.Create(account).Error)
	if !withEvidence {
		return account, nil
	}
	return account, createRegistrationInboundEvidence(
		t,
		app,
		account,
		pendingSince.Add(time.Minute),
	)
}

func createRegistrationInboundEvidence(
	t *testing.T,
	app *App,
	account *models.WhatsAppAccount,
	evidenceAt time.Time,
) *models.Message {
	t.Helper()
	unique := uuid.NewString()
	contact := &models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: evidenceAt, UpdatedAt: evidenceAt},
		OrganizationID:  account.OrganizationID,
		PhoneNumber:     "6011" + strings.ReplaceAll(unique[:12], "-", ""),
		ProfileName:     "Reconciliation Evidence Sender",
		WhatsAppAccount: account.Name,
	}
	require.NoError(t, app.DB.Create(contact).Error)
	message := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New(), CreatedAt: evidenceAt, UpdatedAt: evidenceAt},
		OrganizationID:    account.OrganizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		WhatsAppMessageID: "wamid.reconciliation." + unique,
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "registration evidence",
		Status:            models.MessageStatusReceived,
	}
	require.NoError(t, app.DB.Create(message).Error)
	job := &models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: evidenceAt, UpdatedAt: evidenceAt},
		OrganizationID: account.OrganizationID,
		Kind:           inboundContinuationJobKind,
		AggregateType:  "message",
		AggregateID:    &message.ID,
		RunAt:          evidenceAt,
		Status:         models.ScheduledJobStatusCompleted,
		IdempotencyKey: "registration-reconciliation-evidence:" + message.ID.String(),
		Payload: models.JSONB{
			"phone_number_id": account.PhoneID,
		},
		Version: 1,
	}
	require.NoError(t, app.DB.Create(job).Error)
	return message
}

func TestReconcilePhoneRegistrationActivatesFromExactRecentInboundEvidenceWithoutMetaMutation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	pendingSince := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	account, evidence := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, pendingSince, true,
	)
	original := *account

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(req, "id", account.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.NotContains(t, string(testutil.GetResponseBody(req)), evidence.WhatsAppMessageID,
		"provider message identifiers belong in the internal audit trail, not the client response")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 0, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, original.ID, stored.ID)
	assert.Equal(t, original.Name, stored.Name)
	assert.Equal(t, original.PhoneID, stored.PhoneID)
	assert.Equal(t, original.BusinessID, stored.BusinessID)
	assert.Equal(t, original.APIVersion, stored.APIVersion)
	assert.Equal(t, original.AccessToken, stored.AccessToken)
	assert.Equal(t, original.Pin, stored.Pin)
	assert.Equal(t, original.IsDefaultIncoming, stored.IsDefaultIncoming)
	assert.Equal(t, original.IsDefaultOutgoing, stored.IsDefaultOutgoing)
	assert.Equal(t, original.AutoReadReceipt, stored.AutoReadReceipt)
	assert.Equal(t, original.BusinessCallingEnabled, stored.BusinessCallingEnabled)

	var auditEntry models.AuditLog
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
		org.ID,
		"account",
		account.ID,
		models.AuditActionUpdated,
	).Order("created_at DESC").First(&auditEntry).Error)
	assert.Contains(t, fmt.Sprint(auditEntry.Changes), "registration_reconciliation_evidence")
	assert.Contains(t, fmt.Sprint(auditEntry.Changes), evidence.WhatsAppMessageID)
}

func TestReconcilePhoneRegistrationFailsClosedWithoutPostPendingInboundEvidence(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account, _ := createRegistrationReconciliationFixture(
		t,
		app,
		org.ID,
		phoneID,
		wabaID,
		time.Now().UTC().Add(-10*time.Minute).Truncate(time.Millisecond),
		false,
	)

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(req, "id", account.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "No recent inbound WhatsApp evidence")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 0, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, account.AccessToken, stored.AccessToken)
	assert.Equal(t, account.Pin, stored.Pin)
}

func TestReconcilePhoneRegistrationRequiresEvidenceAfterLatestAmbiguousRegisterRetry(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	meta.registrationStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	pendingSince := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	account, oldEvidence := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, pendingSince, true,
	)

	retryReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(retryReq, "id", account.ID.String())
	testutil.SetAuthContext(retryReq, org.ID, user.ID)
	testutil.SetHeader(retryReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.RegisterPhoneNumber(retryReq))
	testutil.AssertErrorResponse(t, retryReq, fasthttp.StatusBadGateway, "remains pending registration")

	var afterRetry models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&afterRetry).Error)
	assert.Equal(t, "pending_registration", afterRetry.Status)
	assert.True(t, afterRetry.UpdatedAt.After(oldEvidence.CreatedAt), "retry must advance the evidence watermark")

	staleEvidenceReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(staleEvidenceReq, "id", account.ID.String())
	testutil.SetAuthContext(staleEvidenceReq, org.ID, user.ID)
	testutil.SetHeader(staleEvidenceReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(staleEvidenceReq))
	testutil.AssertErrorResponse(t, staleEvidenceReq, fasthttp.StatusConflict, "No recent inbound WhatsApp evidence")

	postRetryAt := time.Now().UTC()
	if !postRetryAt.After(afterRetry.UpdatedAt) {
		postRetryAt = afterRetry.UpdatedAt.Add(time.Microsecond)
	}
	newEvidence := createRegistrationInboundEvidence(t, app, &afterRetry, postRetryAt)
	freshEvidenceReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(freshEvidenceReq, "id", account.ID.String())
	testutil.SetAuthContext(freshEvidenceReq, org.ID, user.ID)
	testutil.SetHeader(freshEvidenceReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(freshEvidenceReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(freshEvidenceReq))
	assert.NotContains(t, string(testutil.GetResponseBody(freshEvidenceReq)), newEvidence.WhatsAppMessageID,
		"provider message identifiers belong in the internal audit trail, not the client response")

	var active models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&active).Error)
	assert.Equal(t, "active", active.Status)
	assert.Equal(t, 1, meta.methodHits(http.MethodPost, "/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 2, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestRegisterPhoneNumberWatermarkIsStrictlyMonotonicAcrossClockSkew(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.registrationStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	futureWatermark := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Microsecond)
	account, _ := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, futureWatermark, false,
	)
	var before models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&before).Error)

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(req, "id", account.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.RegisterPhoneNumber(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadGateway, "remains pending registration")

	var after models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&after).Error)
	assert.Equal(t, "pending_registration", after.Status)
	assert.True(t, after.UpdatedAt.After(before.UpdatedAt))
	assert.Equal(t, before.UpdatedAt.Add(time.Microsecond), after.UpdatedAt)
}

func TestConcurrentRegistrationRetriesOnlyNewestWatermarkCanFinalize(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account, _ := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, time.Now().UTC().Add(-time.Minute), false,
	)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var registerCalls atomic.Int32
	meta.onRegister = func(map[string]string) {
		if registerCalls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
	}
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	})

	newRetryRequest := func() *fastglue.Request {
		req := testutil.NewJSONRequest(t, map[string]any{})
		testutil.SetPathParam(req, "id", account.ID.String())
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
		return req
	}
	firstReq := newRetryRequest()
	firstDone := make(chan error, 1)
	go func() { firstDone <- app.RegisterPhoneNumber(firstReq) }()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first registration retry did not reach Meta")
	}

	secondReq := newRetryRequest()
	require.NoError(t, app.RegisterPhoneNumber(secondReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondReq))
	close(releaseFirst)
	require.NoError(t, <-firstDone)
	testutil.AssertErrorResponse(t, firstReq, fasthttp.StatusConflict, "replaced by a newer request")

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&stored).Error)
	assert.Equal(t, "pending_subscription", stored.Status)
	assert.Equal(t, int32(2), registerCalls.Load())
}

func TestRegistrationReconciliationFailsClosedWhileRetryHasOnlyOlderEvidence(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account, _ := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, time.Now().UTC().Add(-10*time.Minute), true,
	)
	registerEntered := make(chan struct{})
	releaseRegister := make(chan struct{})
	meta.onRegister = func(map[string]string) {
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

	retryReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(retryReq, "id", account.ID.String())
	testutil.SetAuthContext(retryReq, org.ID, user.ID)
	testutil.SetHeader(retryReq, "X-Organization-ID", org.ID.String())
	retryDone := make(chan error, 1)
	go func() { retryDone <- app.RegisterPhoneNumber(retryReq) }()
	select {
	case <-registerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration retry did not reach Meta")
	}

	reconcileReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(reconcileReq, "id", account.ID.String())
	testutil.SetAuthContext(reconcileReq, org.ID, user.ID)
	testutil.SetHeader(reconcileReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(reconcileReq))
	testutil.AssertErrorResponse(t, reconcileReq, fasthttp.StatusConflict, "No recent inbound WhatsApp evidence")

	close(releaseRegister)
	require.NoError(t, <-retryDone)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retryReq))
}

func TestRegistrationRetrySupersedesReconciliationSnapshotBeforeActivation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	meta.registrationStatus = http.StatusBadGateway
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account, _ := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, time.Now().UTC().Add(-10*time.Minute), true,
	)

	subscriptionReadEntered := make(chan struct{})
	releaseSubscriptionRead := make(chan struct{})
	meta.onSubscriptionRead = func() {
		close(subscriptionReadEntered)
		<-releaseSubscriptionRead
	}
	t.Cleanup(func() {
		select {
		case <-releaseSubscriptionRead:
		default:
			close(releaseSubscriptionRead)
		}
	})

	reconcileReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(reconcileReq, "id", account.ID.String())
	testutil.SetAuthContext(reconcileReq, org.ID, user.ID)
	testutil.SetHeader(reconcileReq, "X-Organization-ID", org.ID.String())
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- app.ReconcilePhoneRegistration(reconcileReq) }()
	select {
	case <-subscriptionReadEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration reconciliation did not reach app-subscription proof")
	}

	retryReq := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(retryReq, "id", account.ID.String())
	testutil.SetAuthContext(retryReq, org.ID, user.ID)
	testutil.SetHeader(retryReq, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.RegisterPhoneNumber(retryReq))
	testutil.AssertErrorResponse(t, retryReq, fasthttp.StatusBadGateway, "remains pending registration")

	close(releaseSubscriptionRead)
	require.NoError(t, <-reconcileDone)
	testutil.AssertErrorResponse(t, reconcileReq, fasthttp.StatusConflict, "changed; reload and retry")

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.True(t, stored.UpdatedAt.After(account.UpdatedAt))
	assert.Equal(t, 1, meta.methodHits(http.MethodPost, "/v21.0/"+phoneID+"/register"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
}

func TestReconcilePhoneRegistrationFailsClosedWhenCurrentAppIsNotSubscribed(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.permanentToken = true
	meta.subscribedAppIDs = []string{"990000000000002"}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account, _ := createRegistrationReconciliationFixture(
		t, app, org.ID, phoneID, wabaID, time.Now().UTC().Add(-10*time.Minute), true,
	)

	req := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(req, "id", account.ID.String())
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ReconcilePhoneRegistration(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "not subscribed")

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&stored).Error)
	assert.Equal(t, "pending_registration", stored.Status)
	assert.Equal(t, account.AccessToken, stored.AccessToken)
	assert.Equal(t, account.Pin, stored.Pin)
	assert.Equal(t, 1, meta.methodHits(http.MethodGet, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
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
