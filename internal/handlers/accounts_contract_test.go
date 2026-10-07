package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
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

func TestEmbeddedSignupGeneratedAccountNameUsesFullID(t *testing.T) {
	firstID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	secondID := uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa")
	phoneInfo := &whatsapp.PhoneNumberInfo{
		VerifiedName:       "Synthetic Clinic",
		DisplayPhoneNumber: "+60123456789",
	}

	first, err := embeddedSignupAccountName("", firstID, phoneInfo)
	require.NoError(t, err)
	second, err := embeddedSignupAccountName("", secondID, phoneInfo)
	require.NoError(t, err)
	assert.Equal(t, "Synthetic Clinic (+60123456789) "+firstID.String(), first)
	assert.Equal(t, "Synthetic Clinic (+60123456789) "+secondID.String(), second)
	assert.NotEqual(t, first, second, "a repeated Meta name must not collide across accounts")

	longName, err := embeddedSignupAccountName("", firstID, &whatsapp.PhoneNumberInfo{
		VerifiedName: strings.Repeat("診", 80),
	})
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("診", 63)+" "+firstID.String(), longName)
	assert.Equal(t, 100, utf8.RuneCountInString(longName))

	fallback, err := embeddedSignupAccountName("", firstID, nil)
	require.NoError(t, err)
	assert.Equal(t, "WhatsApp Account "+firstID.String(), fallback)

	explicit, err := embeddedSignupAccountName("  Chosen Account  ", uuid.Nil, phoneInfo)
	require.NoError(t, err)
	assert.Equal(t, "Chosen Account", explicit)
	_, err = embeddedSignupAccountName("", uuid.Nil, phoneInfo)
	require.Error(t, err)
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
	listedDisplay      string
	extraListedPhones  []whatsapp.WABAPhoneNumber
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
		listedDisplay:      "+60123456789",
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
			listed := []map[string]string{{
				"id":                   meta.listedPhoneID,
				"display_phone_number": meta.listedDisplay,
				"verified_name":        "Synthetic Clinic",
				"quality_rating":       "GREEN",
			}}
			for _, phone := range meta.extraListedPhones {
				listed = append(listed, map[string]string{
					"id":                   phone.ID,
					"display_phone_number": phone.DisplayPhoneNumber,
					"verified_name":        phone.VerifiedName,
					"quality_rating":       "GREEN",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": listed})
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
		case strings.HasSuffix(r.URL.Path, "/"+meta.phoneID+"/smb_app_data"):
			var body struct {
				MessagingProduct string `json:"messaging_product"`
				SyncType         string `json:"sync_type"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil ||
				body.MessagingProduct != "whatsapp" ||
				(body.SyncType != "smb_app_state_sync" && body.SyncType != "history") {
				http.Error(w, "invalid synthetic coexistence sync contract", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"messaging_product": "whatsapp",
				"request_id":        "synthetic-" + body.SyncType + "-request",
			})
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

func TestCreateAccountRejectsSMBNumberOutsideCoexistenceEmbeddedSignup(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := createContractAccountRequest(
		t,
		app,
		org.ID,
		user.ID,
		"Manual SMB Bypass",
		phoneID,
		wabaID,
		"synthetic-manual-smb-token",
	)
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "Coexistence Embedded Signup")
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))

	var count int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
		Count(&count).Error)
	assert.Zero(t, count, "manual creation must not persist an SMB account")
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
	shadow := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    org.ID,
		Channel:           models.ChannelWhatsApp,
		Provider:          channelapi.LegacyMetaProvider,
		Name:              "WhatsApp " + account.Name + " [" + account.ID.String() + "]",
		ExternalAccountID: "legacy-account:" + account.ID.String(),
		Status:            models.ChannelAccountStatusSuspended,
		Capabilities:      models.JSONB{"text": true, "replies": true, "service_window": true},
		Config:            models.JSONB{"legacy_read_only": true, "outbound_enabled": false, "reply_route": "chat"},
		Metadata: models.JSONB{
			"legacy_account_id":   account.ID.String(),
			"legacy_account_name": account.Name,
		},
	}
	require.NoError(t, app.DB.Create(shadow).Error)

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
	var storedShadow models.ChannelAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", shadow.ID, org.ID).
		First(&storedShadow).Error)
	assert.Equal(t, "Safe Pending Display Edit", storedShadow.Metadata["legacy_account_name"])
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
		"code":        "claim-during-put",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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

// Under RLS the whole request is one tenant transaction. A rename that also
// changes the account contract must not keep the rename stage's organization
// lock across the Meta subscription call, or every policy fence in the tenant
// (each inbound admission) would wait for that call.
func TestUpdateAccountContractRenameReleasesOrganizationBeforeSubscription(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Contract Before " + uuid.NewString()[:8],
		PhoneID:        "old-phone",
		BusinessID:     "old-waba",
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(account).Error)
	shadow, err := ensureFencedLegacyMetaAccountForTest(app.DB, channelapi.LegacyMetaAccountRef{
		ID: account.ID, OrganizationID: org.ID, Name: account.Name, Status: account.Status,
	})
	require.NoError(t, err)
	app.Config.Database.RLSEnabled = true

	inSubscribe := make(chan struct{})
	release := make(chan struct{})
	var inSubscribeOnce, releaseOnce sync.Once
	releaseSubscribe := func() { releaseOnce.Do(func() { close(release) }) }
	meta.onSubscribe = func() {
		inSubscribeOnce.Do(func() { close(inSubscribe) })
		<-release
	}
	defer releaseSubscribe()

	nextName := "Contract After " + uuid.NewString()[:8]
	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         nextName,
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "synthetic-updated-token",
		"api_version":  "v21.0",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	handled := make(chan error, 1)
	go func() { handled <- app.Tenant((*App).UpdateAccount)(req) }()
	select {
	case <-inSubscribe:
	case handlerErr := <-handled:
		require.Failf(t, "update finished before the Meta subscription", "error: %v", handlerErr)
	case <-time.After(15 * time.Second):
		require.Fail(t, "update never reached the Meta subscription")
	}

	fence := app.DB.Begin()
	require.NoError(t, fence.Error)
	defer func() { _ = fence.Rollback().Error }()
	require.NoError(t, fence.Exec("SET LOCAL lock_timeout = '1s'").Error)
	require.NoError(t, database.LockOrganizationPolicyScope(fence, org.ID),
		"the rename held the organization across the Meta subscription")
	require.NoError(t, fence.Commit().Error)

	releaseSubscribe()
	select {
	case handlerErr := <-handled:
		require.NoError(t, handlerErr)
	case <-time.After(15 * time.Second):
		require.Fail(t, "update did not finish after the Meta subscription returned")
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, nextName, stored.Name)
	assert.Equal(t, "active", stored.Status)
	var storedShadow models.ChannelAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", shadow.ID, org.ID).First(&storedShadow).Error)
	assert.Equal(t, nextName, storedShadow.Metadata["legacy_account_name"])
}

// Under RLS a contract change whose subscription status is superseded during
// the Meta call answers 409 and rolls back the request transaction. The account
// update committed before the call, so moving the default flags must have
// committed with it: the previous default may not stay a default as well.
func TestUpdateAccountSupersededSubscriptionKeepsOneDefaultAccount(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	previousDefault := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Previous Default " + uuid.NewString()[:8], PhoneID: "previous-phone-" + uuid.NewString()[:8], BusinessID: "previous-waba",
		AccessToken: oldToken, APIVersion: "v21.0", Status: "active",
		IsDefaultIncoming: true, IsDefaultOutgoing: true,
	}
	require.NoError(t, app.DB.Create(previousDefault).Error)
	account := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Becoming Default " + uuid.NewString()[:8], PhoneID: "old-phone-" + uuid.NewString()[:8], BusinessID: "old-waba",
		AccessToken: oldToken, APIVersion: "v21.0", Status: "active",
	}
	require.NoError(t, app.DB.Create(account).Error)
	_, err = ensureFencedLegacyMetaAccountForTest(app.DB, channelapi.LegacyMetaAccountRef{
		ID: account.ID, OrganizationID: org.ID, Name: account.Name, Status: account.Status,
	})
	require.NoError(t, err)
	app.Config.Database.RLSEnabled = true
	meta.onSubscribe = func() {
		// A concurrent status write supersedes this request's subscription claim.
		_ = app.DB.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, org.ID).
			Update("status", "subscription_failed").Error
	}

	nextName := "Now Default " + uuid.NewString()[:8]
	req := testutil.NewJSONRequest(t, map[string]any{
		"name":                nextName,
		"phone_id":            phoneID,
		"business_id":         wabaID,
		"access_token":        "synthetic-updated-token",
		"api_version":         "v21.0",
		"is_default_incoming": true,
		"is_default_outgoing": true,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.Tenant((*App).UpdateAccount)(req))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, nextName, stored.Name)
	assert.NotEqual(t, oldToken, stored.AccessToken)
	assert.True(t, stored.IsDefaultIncoming)
	assert.True(t, stored.IsDefaultOutgoing)
	var incomingDefaults, outgoingDefaults int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND is_default_incoming = ?", org.ID, true).Count(&incomingDefaults).Error)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND is_default_outgoing = ?", org.ID, true).Count(&outgoingDefaults).Error)
	assert.EqualValues(t, 1, incomingDefaults)
	assert.EqualValues(t, 1, outgoingDefaults)

	// The committed update keeps its audit row although the request failed.
	changed := accountAuditFields(t, app, org.ID, account.ID)
	require.Len(t, changed, 1)
	assert.Equal(t, nextName, changed[0]["name"])
	assert.Equal(t, "********", changed[0]["access_token"], "the token change is audited masked")
	assert.Equal(t, true, changed[0]["is_default_incoming"])
}

// accountAuditFields returns, per account audit row in creation order, each
// changed field's new value.
func accountAuditFields(t *testing.T, app *App, orgID, accountID uuid.UUID) []map[string]any {
	t.Helper()
	var audits []models.AuditLog
	require.NoError(t, app.DB.
		Where("organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
			orgID, "account", accountID, models.AuditActionUpdated).
		Order("created_at").Find(&audits).Error)
	fields := make([]map[string]any, 0, len(audits))
	for _, entry := range audits {
		changed := map[string]any{}
		for _, change := range entry.Changes {
			if change, ok := change.(map[string]any); ok {
				if field, ok := change["field"].(string); ok {
					changed[field] = change["new_value"]
				}
			}
		}
		fields = append(fields, changed)
	}
	return fields
}

func createDefaultMoveAccount(t *testing.T, app *App, orgID uuid.UUID, label string, isDefault bool) *models.WhatsAppAccount {
	t.Helper()
	token, err := appcrypto.Encrypt("synthetic-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	phoneID, wabaID := contractGraphIDs()
	account := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: label + " " + uuid.NewString()[:8], PhoneID: phoneID, BusinessID: wabaID,
		AccessToken: token, APIVersion: "v21.0", Status: "active",
		IsDefaultIncoming: isDefault, IsDefaultOutgoing: isDefault,
	}
	require.NoError(t, app.DB.Create(account).Error)
	return account
}

func makeDefaultAccountRequest(t *testing.T, orgID, userID, accountID uuid.UUID) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{
		"is_default_incoming": true,
		"is_default_outgoing": true,
	})
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetPathParam(req, "id", accountID.String())
	return req
}

func countDefaultAccounts(t *testing.T, app *App, orgID uuid.UUID) (incoming, outgoing []uuid.UUID) {
	t.Helper()
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND is_default_incoming = ?", orgID, true).Pluck("id", &incoming).Error)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).Pluck("id", &outgoing).Error)
	return incoming, outgoing
}

// The default move clears both flags in one UPDATE. Updating the same row a
// second time in one transaction re-runs its foreign-key check, which takes
// the organization FOR KEY SHARE and so waits behind an admission's policy
// fence (organization FOR UPDATE) while the move holds that row. The move
// must finish while an admission holds its fence.
func TestUpdateAccountDefaultMoveDoesNotWaitForTheOrganization(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
	mover := createDefaultMoveAccount(t, app, org.ID, "Mover", false)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", mover.ID).
		Updates(map[string]any{"is_default_incoming": true, "is_default_outgoing": true}).Error)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admission := app.DB.WithContext(ctx).Begin()
	require.NoError(t, admission.Error)
	defer func() { _ = admission.Rollback().Error }()
	require.NoError(t, database.LockOrganizationPolicyScope(admission, org.ID))

	moved := make(chan error, 1)
	go func() { moved <- app.clearOtherWhatsAppAccountDefaults(org.ID, mover.ID, true, true) }()
	select {
	case err := <-moved:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "the default move waited for the admission's organization lock")
	}
	require.NoError(t, admission.Commit().Error)
	incoming, outgoing := countDefaultAccounts(t, app, org.ID)
	assert.Equal(t, []uuid.UUID{mover.ID}, incoming)
	assert.Equal(t, []uuid.UUID{mover.ID}, outgoing)
	assert.NotContains(t, incoming, previous.ID)
}

// The previous incoming and outgoing defaults can be different accounts, and
// sends hold an account row FOR SHARE across their Meta call. While the clear
// waits for such a send on one of them, it must not keep the other one locked:
// an admission on that account would wait behind it while it holds the
// tenant's policy fence.
func TestUpdateAccountDefaultClearWaitingForASendKeepsNoOtherAccountLocked(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	incomingDefault := createDefaultMoveAccount(t, app, org.ID, "Incoming Default", false)
	outgoingDefault := createDefaultMoveAccount(t, app, org.ID, "Outgoing Default", false)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", incomingDefault.ID).
		Update("is_default_incoming", true).Error)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", outgoingDefault.ID).
		Update("is_default_outgoing", true).Error)
	mover := createDefaultMoveAccount(t, app, org.ID, "Mover", false)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", mover.ID).
		Updates(map[string]any{"is_default_incoming": true, "is_default_outgoing": true}).Error)

	for _, sent := range []*models.WhatsAppAccount{incomingDefault, outgoingDefault} {
		other := incomingDefault
		if sent == incomingDefault {
			other = outgoingDefault
		}
		t.Run(sent.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Reset the flags for this round.
			require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", incomingDefault.ID).
				Update("is_default_incoming", true).Error)
			require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", outgoingDefault.ID).
				Update("is_default_outgoing", true).Error)
			send := app.DB.WithContext(ctx).Begin()
			require.NoError(t, send.Error)
			defer func() { _ = send.Rollback().Error }()
			var held []models.WhatsAppAccount
			require.NoError(t, send.Clauses(clause.Locking{Strength: "SHARE"}).Select("id").
				Where("id = ?", sent.ID).Find(&held).Error)
			var sendPID int
			require.NoError(t, send.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&sendPID).Error)

			cleared := make(chan error, 1)
			go func() { cleared <- app.clearOtherWhatsAppAccountDefaults(org.ID, mover.ID, true, true) }()
			require.Eventually(t, func() bool {
				var waiting bool
				return app.DB.Raw(
					"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
					sendPID,
				).Scan(&waiting).Error == nil && waiting
			}, 10*time.Second, 5*time.Millisecond, "the clear must wait for the send")

			probe := app.DB.WithContext(ctx).Begin()
			require.NoError(t, probe.Error)
			var probed []models.WhatsAppAccount
			probeErr := probe.Clauses(clause.Locking{Strength: "SHARE", Options: "NOWAIT"}).Select("id").
				Where("id = ?", other.ID).Find(&probed).Error
			_ = probe.Rollback().Error
			require.NoError(t, probeErr, "the clear kept another account locked while it waited for a send")

			require.NoError(t, send.Commit().Error)
			select {
			case err := <-cleared:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				require.Fail(t, "the clear did not finish after the send")
			}
			incoming, outgoing := countDefaultAccounts(t, app, org.ID)
			assert.Equal(t, []uuid.UUID{mover.ID}, incoming)
			assert.Equal(t, []uuid.UUID{mover.ID}, outgoing)
		})
	}
}

// Two requests that each make a different account the default commit their own
// flags first and then clear the others. Unserialized, or clearing regardless
// of the mover's own flag, they cleared each other and left no default at all.
// Here both have committed their flags before either clear runs; exactly one
// default of each kind must survive.
func TestUpdateAccountConcurrentDefaultMovesLeaveOneDefault(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
			first := createDefaultMoveAccount(t, app, org.ID, "First Mover", false)
			second := createDefaultMoveAccount(t, app, org.ID, "Second Mover", false)
			app.Config.Database.RLSEnabled = rlsEnabled

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			moves := app.DB.WithContext(ctx).Begin()
			require.NoError(t, moves.Error)
			defer func() { _ = moves.Rollback().Error }()
			require.NoError(t, moves.Exec(
				"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
				whatsappDefaultAccountMoveKey(org.ID),
			).Error)
			var movesPID int
			require.NoError(t, moves.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&movesPID).Error)

			requests := []*fastglue.Request{
				makeDefaultAccountRequest(t, org.ID, user.ID, first.ID),
				makeDefaultAccountRequest(t, org.ID, user.ID, second.ID),
			}
			handled := make(chan error, len(requests))
			for _, req := range requests {
				go func(req *fastglue.Request) { handled <- app.Tenant((*App).UpdateAccount)(req) }(req)
			}
			require.Eventually(t, func() bool {
				var waiting int64
				return app.DB.Raw(
					"SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid))",
					movesPID,
				).Scan(&waiting).Error == nil && waiting == int64(len(requests))
			}, 15*time.Second, 10*time.Millisecond, "both requests must commit their flags and wait to clear the others")
			require.NoError(t, moves.Commit().Error)
			for range requests {
				select {
				case handlerErr := <-handled:
					require.NoError(t, handlerErr)
				case <-time.After(15 * time.Second):
					require.Fail(t, "a default move did not finish")
				}
			}
			for _, req := range requests {
				assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			}

			incoming, outgoing := countDefaultAccounts(t, app, org.ID)
			require.Len(t, incoming, 1, "exactly one incoming default must survive")
			require.Len(t, outgoing, 1, "exactly one outgoing default must survive")
			assert.Equal(t, incoming[0], outgoing[0])
			assert.NotEqual(t, previous.ID, incoming[0])
		})
	}
}

// failDefaultClearOf makes every UPDATE that clears account's incoming
// default flag fail until the returned function drops the trigger.
func failDefaultClearOf(t *testing.T, app *App, accountID uuid.UUID) func() {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	functionName := "fail_default_clear_" + suffix
	triggerName := functionName + "_trigger"
	dropped := false
	drop := func() {
		if !dropped {
			require.NoError(t, dropLegacyTestTrigger(app.DB, "whatsapp_accounts", triggerName, functionName))
			dropped = true
		}
	}
	t.Cleanup(drop)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF OLD.id = '%s'::uuid AND OLD.is_default_incoming AND NOT NEW.is_default_incoming THEN
				RAISE EXCEPTION 'synthetic default clear failure';
			END IF;
			RETURN NEW;
		END;
		$$`, functionName, accountID)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE UPDATE ON whatsapp_accounts FOR EACH ROW EXECUTE FUNCTION %s()",
		triggerName, functionName,
	)).Error)
	return drop
}

// A failed clear of the previous default must not report success, and an
// identical retry must repair it: the clear runs whenever the request sets the
// flag, not only when the flag changes. With a contract change the request has
// also subscribed the account; its status must stay committed, or the retry
// is refused while recovery owns a pending_subscription account.
func TestUpdateAccountRetryRepairsAFailedDefaultClear(t *testing.T) {
	for _, contractChange := range []bool{false, true} {
		for _, rlsEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("contract=%v/rls=%v", contractChange, rlsEnabled), func(t *testing.T) {
				phoneID, wabaID := contractGraphIDs()
				meta := newWhatsAppContractMeta(t, phoneID, wabaID)
				app := newWhatsAppContractApp(t, meta)
				org := testutil.CreateTestOrganization(t, app.DB)
				user := contractWriter(t, app, org.ID)
				previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
				account := createDefaultMoveAccount(t, app, org.ID, "Next Default", false)
				app.Config.Database.RLSEnabled = rlsEnabled
				drop := failDefaultClearOf(t, app, previous.ID)

				body := map[string]any{"is_default_incoming": true, "is_default_outgoing": true}
				if contractChange {
					body["phone_id"] = phoneID
					body["business_id"] = wabaID
					body["access_token"] = "synthetic-updated-token"
					body["api_version"] = "v21.0"
				}
				put := func() *fastglue.Request {
					req := testutil.NewJSONRequest(t, body)
					testutil.SetAuthContext(req, org.ID, user.ID)
					testutil.SetPathParam(req, "id", account.ID.String())
					require.NoError(t, app.Tenant((*App).UpdateAccount)(req))
					return req
				}

				req := put()
				require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
				assert.Contains(t, string(testutil.GetResponseBody(req)), errWhatsAppDefaultAccountClearMessage)
				incoming, outgoing := countDefaultAccounts(t, app, org.ID)
				assert.Len(t, incoming, 2, "the update committed; only the clear failed")
				assert.Len(t, outgoing, 2)
				var stored models.WhatsAppAccount
				require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
				audits := accountAuditFields(t, app, org.ID, account.ID)
				if contractChange {
					assert.Equal(t, "active", stored.Status, "the subscription status stays committed")
					require.Len(t, audits, 2, "the update and the subscription status are both audited")
					assert.Equal(t, "active", audits[1]["status"])
				} else {
					assert.Len(t, audits, 1, "the committed update is audited")
				}

				drop()
				retry := put()
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry), string(testutil.GetResponseBody(retry)))
				incoming, outgoing = countDefaultAccounts(t, app, org.ID)
				assert.Equal(t, []uuid.UUID{account.ID}, incoming)
				assert.Equal(t, []uuid.UUID{account.ID}, outgoing)
			})
		}
	}
}

// A contract change whose subscription fails leaves the account in recovery,
// which refuses another credential change. If the default clear failed too, a
// 500 would invite a retry that is refused, so the request reports both as
// warnings; saving the account again without credentials repairs the clear.
func TestUpdateAccountFailedSubscriptionAndDefaultClearAreBothRepairable(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			meta.subscriptionStatus = http.StatusBadGateway
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
			account := createDefaultMoveAccount(t, app, org.ID, "Next Default", false)
			app.Config.Database.RLSEnabled = rlsEnabled
			drop := failDefaultClearOf(t, app, previous.ID)

			req := testutil.NewJSONRequest(t, map[string]any{
				"phone_id":            phoneID,
				"business_id":         wabaID,
				"access_token":        "synthetic-updated-token",
				"api_version":         "v21.0",
				"is_default_incoming": true,
				"is_default_outgoing": true,
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", account.ID.String())
			require.NoError(t, app.Tenant((*App).UpdateAccount)(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
			body := string(testutil.GetResponseBody(req))
			assert.Contains(t, body, "Webhook subscription failed")
			assert.Contains(t, body, errWhatsAppDefaultAccountClearWarning)
			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
			assert.Equal(t, "subscription_failed", stored.Status)
			incoming, _ := countDefaultAccounts(t, app, org.ID)
			assert.Len(t, incoming, 2)

			drop()
			repair := makeDefaultAccountRequest(t, org.ID, user.ID, account.ID)
			require.NoError(t, app.Tenant((*App).UpdateAccount)(repair))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(repair), string(testutil.GetResponseBody(repair)))
			incoming, outgoing := countDefaultAccounts(t, app, org.ID)
			assert.Equal(t, []uuid.UUID{account.ID}, incoming)
			assert.Equal(t, []uuid.UUID{account.ID}, outgoing)
		})
	}
}

// CreateAccount makes the new account the default with its subscription
// status, audited together, and then clears the other accounts. Repeating the
// create would only collide with the saved account, so a failed clear is a
// warning, and saving the new account with the default option repairs it.
func TestCreateAccountDefaultIsAuditedAndAFailedClearIsAWarning(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
			app.Config.Database.RLSEnabled = rlsEnabled
			drop := failDefaultClearOf(t, app, previous.ID)

			create := testutil.NewJSONRequest(t, map[string]any{
				"name":                "Created Default " + uuid.NewString()[:8],
				"phone_id":            phoneID,
				"business_id":         wabaID,
				"access_token":        "synthetic-created-token",
				"api_version":         "v21.0",
				"is_default_incoming": true,
				"is_default_outgoing": true,
			})
			testutil.SetAuthContext(create, org.ID, user.ID)
			require.NoError(t, app.Tenant((*App).CreateAccount)(create))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(create), string(testutil.GetResponseBody(create)))
			assert.Contains(t, string(testutil.GetResponseBody(create)), errWhatsAppDefaultAccountClearWarning)
			var created models.WhatsAppAccount
			require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&created).Error)
			assert.Equal(t, "active", created.Status)
			assert.True(t, created.IsDefaultIncoming)
			assert.True(t, created.IsDefaultOutgoing)
			var creations int64
			require.NoError(t, app.DB.Model(&models.AuditLog{}).
				Where("organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
					org.ID, "account", created.ID, models.AuditActionCreated).
				Count(&creations).Error)
			assert.EqualValues(t, 1, creations)
			updates := accountAuditFields(t, app, org.ID, created.ID)
			require.Len(t, updates, 1, "the subscription status and the default flags are audited together")
			assert.Equal(t, "active", updates[0]["status"])
			assert.Equal(t, true, updates[0]["is_default_incoming"])
			assert.Equal(t, true, updates[0]["is_default_outgoing"])

			drop()
			repair := makeDefaultAccountRequest(t, org.ID, user.ID, created.ID)
			require.NoError(t, app.Tenant((*App).UpdateAccount)(repair))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(repair), string(testutil.GetResponseBody(repair)))
			incoming, outgoing := countDefaultAccounts(t, app, org.ID)
			assert.Equal(t, []uuid.UUID{created.ID}, incoming)
			assert.Equal(t, []uuid.UUID{created.ID}, outgoing)
		})
	}
}

// Random rounds of two to five concurrent default moves, some of them on
// accounts in a recovery status (the pending update path), must each leave
// exactly one default of each kind.
func TestUpdateAccountRandomConcurrentDefaultMovesKeepOneDefault(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			accounts := []*models.WhatsAppAccount{createDefaultMoveAccount(t, app, org.ID, "Mover 0", true)}
			for index := 1; index < 5; index++ {
				accounts = append(accounts, createDefaultMoveAccount(t, app, org.ID, fmt.Sprintf("Mover %d", index), false))
			}
			require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
				Where("id IN ?", []uuid.UUID{accounts[3].ID, accounts[4].ID}).
				Update("status", "pending_subscription").Error)
			app.Config.Database.RLSEnabled = rlsEnabled

			random := mathrand.New(mathrand.NewPCG(uint64(len(t.Name())), 227))
			for round := 0; round < 25; round++ {
				movers := 2 + random.IntN(4)
				var group sync.WaitGroup
				requests := make([]*fastglue.Request, movers)
				for index := range requests {
					requests[index] = makeDefaultAccountRequest(t, org.ID, user.ID, accounts[random.IntN(len(accounts))].ID)
					group.Add(1)
					go func(req *fastglue.Request) {
						defer group.Done()
						assert.NoError(t, app.Tenant((*App).UpdateAccount)(req))
					}(requests[index])
				}
				group.Wait()
				// Two moves of the same account in one round race on its
				// updated_at fence; the loser answers 409 and changes nothing.
				for _, req := range requests {
					assert.Contains(t, []int{fasthttp.StatusOK, fasthttp.StatusConflict},
						testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
				}
				incoming, outgoing := countDefaultAccounts(t, app, org.ID)
				require.Len(t, incoming, 1, "round %d left %d incoming defaults", round, len(incoming))
				require.Len(t, outgoing, 1, "round %d left %d outgoing defaults", round, len(outgoing))
			}
		})
	}
}

// CreateAccount moves the default flags through the same per-organization
// move key as UpdateAccount, after its row commits. A create and an update
// that each make their account the default, both committed before either
// clears, must leave exactly one default of each kind.
func TestCreateAccountDefaultRacingAnUpdateMoveLeavesOneDefault(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			previous := createDefaultMoveAccount(t, app, org.ID, "Previous Default", true)
			mover := createDefaultMoveAccount(t, app, org.ID, "Update Mover", false)
			app.Config.Database.RLSEnabled = rlsEnabled

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			moves := app.DB.WithContext(ctx).Begin()
			require.NoError(t, moves.Error)
			defer func() { _ = moves.Rollback().Error }()
			require.NoError(t, moves.Exec(
				"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
				whatsappDefaultAccountMoveKey(org.ID),
			).Error)
			var movesPID int
			require.NoError(t, moves.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&movesPID).Error)

			create := testutil.NewJSONRequest(t, map[string]any{
				"name":                "Created Default " + uuid.NewString()[:8],
				"phone_id":            phoneID,
				"business_id":         wabaID,
				"access_token":        "synthetic-created-token",
				"api_version":         "v21.0",
				"is_default_incoming": true,
				"is_default_outgoing": true,
			})
			testutil.SetAuthContext(create, org.ID, user.ID)
			update := makeDefaultAccountRequest(t, org.ID, user.ID, mover.ID)
			handled := make(chan error, 2)
			go func() { handled <- app.Tenant((*App).CreateAccount)(create) }()
			go func() { handled <- app.Tenant((*App).UpdateAccount)(update) }()
			require.Eventually(t, func() bool {
				var waiting int64
				return app.DB.Raw(
					"SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid))",
					movesPID,
				).Scan(&waiting).Error == nil && waiting == 2
			}, 15*time.Second, 10*time.Millisecond, "both requests must commit their rows and wait to clear the others")
			require.NoError(t, moves.Commit().Error)
			for range 2 {
				select {
				case handlerErr := <-handled:
					require.NoError(t, handlerErr)
				case <-time.After(15 * time.Second):
					require.Fail(t, "a default move did not finish")
				}
			}
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(create), string(testutil.GetResponseBody(create)))
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(update), string(testutil.GetResponseBody(update)))

			incoming, outgoing := countDefaultAccounts(t, app, org.ID)
			require.Len(t, incoming, 1, "exactly one incoming default must survive")
			require.Len(t, outgoing, 1, "exactly one outgoing default must survive")
			assert.Equal(t, incoming[0], outgoing[0])
			assert.NotEqual(t, previous.ID, incoming[0])
		})
	}
}

// installPlatformComplianceWriteGuard installs tenant RLS, and with it the
// platform-compliance write guard (organizations FOR SHARE on every insert),
// for a synthetic runtime role. The test connection is a superuser, so RLS
// itself does not apply to it.
func installPlatformComplianceWriteGuard(t *testing.T, app *App) {
	t.Helper()
	runtimeRole := "rereply_guard_" + uuid.NewString()[:8]
	require.NoError(t, app.DB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD 'synthetic%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole, uuid.NewString()[:8],
	)).Error)
	t.Cleanup(func() {
		// Best-effort, so a test failure is not hidden by teardown.
		_ = database.RemoveTenantRLS(app.DB)
		_ = app.DB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = app.DB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
	})
	require.NoError(t, database.ApplyTenantRLS(app.DB, runtimeRole))
}

// After a contract change's Meta subscription, the status write and its audit
// row commit in their own transaction. With the platform-compliance write
// guard the audit INSERT locks the organization, so that transaction must take
// the organization before the account row too, or it deadlocks with a
// Coexistence admission (policy fence, organization FOR UPDATE, then the
// account row FOR SHARE). Here an admission takes its fence while Meta is
// called; the status write must wait for the organization without having
// touched the account row.
func TestUpdateAccountSubscriptionStatusTakesTheOrganizationBeforeTheAccountRow(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			installPlatformComplianceWriteGuard(t, app)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			account := createDefaultMoveAccount(t, app, org.ID, "Resubscribing", false)
			app.Config.Database.RLSEnabled = rlsEnabled

			inSubscribe := make(chan struct{})
			release := make(chan struct{})
			var inSubscribeOnce, releaseOnce sync.Once
			releaseSubscribe := func() { releaseOnce.Do(func() { close(release) }) }
			meta.onSubscribe = func() {
				inSubscribeOnce.Do(func() { close(inSubscribe) })
				<-release
			}
			defer releaseSubscribe()

			req := testutil.NewJSONRequest(t, map[string]any{
				"phone_id":     phoneID,
				"business_id":  wabaID,
				"access_token": "synthetic-rotated-token",
				"api_version":  "v21.0",
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetPathParam(req, "id", account.ID.String())
			var handlerErr error
			handled := make(chan struct{})
			go func() {
				defer close(handled)
				handlerErr = app.Tenant((*App).UpdateAccount)(req)
			}()
			select {
			case <-inSubscribe:
			case <-handled:
				require.Failf(t, "update finished before the Meta subscription", "error: %v", handlerErr)
			case <-time.After(15 * time.Second):
				require.Fail(t, "update never reached the Meta subscription")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			admission := app.DB.WithContext(ctx).Begin()
			require.NoError(t, admission.Error)
			defer func() {
				_ = admission.Rollback().Error
				releaseSubscribe()
				select {
				case <-handled:
				case <-time.After(15 * time.Second):
					t.Error("update did not terminate")
				}
			}()
			require.NoError(t, database.LockOrganizationPolicyScope(admission, org.ID))
			var admissionPID int
			require.NoError(t, admission.Session(&gorm.Session{NewDB: true}).
				Raw("SELECT pg_backend_pid()").Scan(&admissionPID).Error)
			releaseSubscribe()
			require.Eventually(t, func() bool {
				var waiting bool
				return app.DB.Raw(
					"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
					admissionPID,
				).Scan(&waiting).Error == nil && waiting
			}, 10*time.Second, 5*time.Millisecond, "the status write must wait for the admission's organization lock")

			var held []models.WhatsAppAccount
			require.NoError(t, admission.Clauses(clause.Locking{Strength: "SHARE", Options: "NOWAIT"}).
				Select("id").Where("id = ? AND organization_id = ?", account.ID, org.ID).Find(&held).Error,
				"the status write locked the account row before the organization")
			require.NoError(t, admission.Commit().Error)
			select {
			case <-handled:
				require.NoError(t, handlerErr)
			case <-time.After(15 * time.Second):
				require.Fail(t, "update did not finish after the admission committed")
			}
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
			assert.Equal(t, "active", stored.Status)
		})
	}
}

// An update that does not rename the account still writes its audit row in
// the commit transaction, and with the platform-compliance write guard that
// INSERT locks the organization FOR SHARE. Taken after the account row, that
// inverted a Coexistence admission's order (policy fence, organization FOR
// UPDATE, then the account row FOR SHARE) and deadlocked. Here an admission
// holds its fence while the update starts: the update must wait for the
// organization without having touched the account row, so the admission can
// still read that row.
func TestAccountUpdateAndDeleteTakeOrganizationBeforeAccountRow(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, rlsEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("delete=%v/rls=%v", deleting, rlsEnabled), func(t *testing.T) {
				phoneID, wabaID := contractGraphIDs()
				meta := newWhatsAppContractMeta(t, phoneID, wabaID)
				app := newWhatsAppContractApp(t, meta)
				installPlatformComplianceWriteGuard(t, app)
				org := testutil.CreateTestOrganization(t, app.DB)
				user := integrationTestUser(t, app, org.ID, "accounts:read", "accounts:write", "accounts:delete")
				account := createDefaultMoveAccount(t, app, org.ID, "Unrenamed", false)
				app.Config.Database.RLSEnabled = rlsEnabled

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				admission := app.DB.WithContext(ctx).Begin()
				require.NoError(t, admission.Error)
				defer func() { _ = admission.Rollback().Error }()
				require.NoError(t, database.LockOrganizationPolicyScope(admission, org.ID))
				var admissionPID int
				require.NoError(t, admission.Session(&gorm.Session{NewDB: true}).
					Raw("SELECT pg_backend_pid()").Scan(&admissionPID).Error)

				req := testutil.NewJSONRequest(t, map[string]any{"auto_read_receipt": true})
				testutil.SetAuthContext(req, org.ID, user.ID)
				testutil.SetPathParam(req, "id", account.ID.String())
				var handlerErr error
				handled := make(chan struct{})
				go func() {
					defer close(handled)
					if deleting {
						handlerErr = app.Tenant((*App).DeleteAccount)(req)
					} else {
						handlerErr = app.Tenant((*App).UpdateAccount)(req)
					}
				}()
				defer func() {
					_ = admission.Rollback().Error
					select {
					case <-handled:
					case <-time.After(15 * time.Second):
						t.Error("update did not terminate")
					}
				}()
				require.Eventually(t, func() bool {
					var waiting bool
					return app.DB.Raw(
						"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
						admissionPID,
					).Scan(&waiting).Error == nil && waiting
				}, 10*time.Second, 5*time.Millisecond, "the update must wait for the admission's organization lock")

				// The admission's next step: the account row, FOR SHARE.
				var held []models.WhatsAppAccount
				require.NoError(t, admission.Clauses(clause.Locking{Strength: "SHARE", Options: "NOWAIT"}).
					Select("id").Where("id = ? AND organization_id = ?", account.ID, org.ID).Find(&held).Error,
					"the update locked the account row before the organization")
				require.NoError(t, admission.Commit().Error)
				select {
				case <-handled:
					require.NoError(t, handlerErr)
				case <-time.After(15 * time.Second):
					require.Fail(t, "update did not finish after the admission committed")
				}
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
				var stored models.WhatsAppAccount
				require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
				if deleting {
					assert.True(t, stored.DeletedAt.Valid)
				} else {
					assert.True(t, stored.AutoReadReceipt)
				}
				if deleting {
					var audits int64
					require.NoError(t, app.DB.Model(&models.AuditLog{}).Where(
						"organization_id = ? AND resource_type = ? AND resource_id = ? AND action = ?",
						org.ID, "account", account.ID, models.AuditActionDeleted).Count(&audits).Error)
					assert.EqualValues(t, 1, audits)
				} else {
					assert.Len(t, accountAuditFields(t, app, org.ID, account.ID), 1)
				}
			})
		}
	}
}

// Sends hold their account row FOR SHARE across the Meta call. A rename that
// waits for that row must not hold the policy fence key or the organization
// meanwhile, or every inbound admission in the tenant would wait for the send.
func TestUpdateAccountRenameWaitingForASendLeavesPolicyFenceAvailable(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	token, err := appcrypto.Encrypt("synthetic-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Sending " + uuid.NewString()[:8], PhoneID: phoneID, BusinessID: wabaID,
		AccessToken: token, APIVersion: "v21.0", Status: "active",
	}
	require.NoError(t, app.DB.Create(account).Error)
	shadow, err := ensureFencedLegacyMetaAccountForTest(app.DB, channelapi.LegacyMetaAccountRef{
		ID: account.ID, OrganizationID: org.ID, Name: account.Name, Status: account.Status,
	})
	require.NoError(t, err)
	app.Config.Database.RLSEnabled = true

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	send := app.DB.WithContext(ctx).Begin()
	require.NoError(t, send.Error)
	defer func() { _ = send.Rollback().Error }()
	var held models.WhatsAppAccount
	require.NoError(t, send.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&held).Error)
	var sendPID int
	require.NoError(t, send.Session(&gorm.Session{NewDB: true}).Raw("SELECT pg_backend_pid()").Scan(&sendPID).Error)

	nextName := "Renamed " + uuid.NewString()[:8]
	req := testutil.NewJSONRequest(t, map[string]any{"name": nextName})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	var handlerErr error
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		handlerErr = app.Tenant((*App).UpdateAccount)(req)
	}()
	defer func() {
		_ = send.Rollback().Error
		select {
		case <-handled:
		case <-time.After(15 * time.Second):
			t.Error("rename did not terminate")
		}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		return app.DB.Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid)))",
			sendPID,
		).Scan(&waiting).Error == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "the rename must wait for the send's account lock")

	fence := app.DB.Begin()
	require.NoError(t, fence.Error)
	defer func() { _ = fence.Rollback().Error }()
	require.NoError(t, fence.Exec("SET LOCAL lock_timeout = '1s'").Error)
	require.NoError(t, database.LockOrganizationPolicyScope(fence, org.ID),
		"the rename held the policy fence while it waited for the send")
	// Admission must also be able to lock this same shadow while the rename
	// waits on the native account. Holding only the fence was insufficient.
	var available models.ChannelAccount
	require.NoError(t, fence.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
		Where("id = ?", shadow.ID).First(&available).Error,
		"rename held the shadow while waiting for a send's account row")
	require.NoError(t, fence.Commit().Error)

	require.NoError(t, send.Commit().Error)
	select {
	case <-handled:
		require.NoError(t, handlerErr)
	case <-time.After(15 * time.Second):
		require.Fail(t, "rename did not finish after the send committed")
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, nextName, stored.Name)
}

func TestUpdateAccountRejectsSMBCredentialRefreshOutsideEmbeddedSignup(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-existing-smb-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Existing Coexistence Account",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         account.Name,
		"access_token": "synthetic-manual-refresh-token",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "refreshed through Embedded Signup")
	assert.Zero(t, meta.totalHits(), "an SMB manual refresh must fail before any Meta call")

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.Equal(t, "active", stored.Status)
}

func TestUpdateAccountRejectsManualClassicToSMBTransition(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-existing-classic-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Existing Classic Account",
		PhoneID:        "old-classic-phone",
		BusinessID:     "old-classic-waba",
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":         account.Name,
		"phone_id":     phoneID,
		"business_id":  wabaID,
		"access_token": "synthetic-manual-transition-token",
		"api_version":  "v21.0",
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "Coexistence Embedded Signup")
	assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&stored).Error)
	assert.Equal(t, "old-classic-phone", stored.PhoneID)
	assert.Equal(t, "old-classic-waba", stored.BusinessID)
	assert.Equal(t, oldToken, stored.AccessToken)
	assert.False(t, stored.IsSMB)
}

func TestEmbeddedSignupSuppliedIDsCannotBypassRelationshipValidation(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.listedPhoneID = "999000000000003"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-authorization-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-ungranted-waba-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-ungranted-messaging-waba-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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

func TestEmbeddedSignupRequiresExplicitConnectionModeBeforeMeta(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)

	for _, mode := range []any{nil, "", "unexpected"} {
		body := map[string]any{
			"code":     "synthetic-mode-validation-code",
			"phone_id": phoneID,
			"waba_id":  wabaID,
		}
		if mode != nil {
			body["signup_mode"] = mode
		}
		req := testutil.NewJSONRequest(t, body)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
		require.NoError(t, app.ExchangeToken(req))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "signup_mode")
	}
	assert.Zero(t, meta.totalHits(), "invalid or missing mode must fail before exchanging the authorization code")
}

func TestEmbeddedSignupRejectsProviderModeMismatchBeforeMutation(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		mode        string
		providerSMB bool
	}{
		{name: "coexistence selected but Meta returns classic", mode: "coexistence"},
		{name: "classic selected but Meta returns coexistence", mode: "classic", providerSMB: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			if testCase.providerSMB {
				meta.phoneIsOnBizApp = true
				meta.phonePlatformType = "SMB_CLOUD_API"
			}
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)

			req := testutil.NewJSONRequest(t, map[string]any{
				"code":        "synthetic-mode-mismatch-code",
				"signup_mode": testCase.mode,
				"phone_id":    phoneID,
				"waba_id":     wabaID,
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
			require.NoError(t, app.ExchangeToken(req))
			testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "does not match")

			assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"))
			assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
			assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/smb_app_data"))
			var count int64
			require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).
				Count(&count).Error)
			assert.Zero(t, count)
		})
	}
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
		"code":        "synthetic-durable-pin-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "first-inflight-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "second-inflight-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-authoritative-smb-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, 1, meta.hit("/v21.0/"+phoneID), "embedded signup must reuse the authoritative validation read")
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 2, meta.hit("/v21.0/"+phoneID+"/smb_app_data"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
	assert.True(t, stored.IsSMB)
	assert.Empty(t, stored.Pin)
	assert.Equal(t, "active", stored.Status)
	var coexistence models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		stored.ID,
	).First(&coexistence).Error)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, coexistence.ContactSyncStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, coexistence.HistorySyncStatus)
}

type embeddedSignupBudgetTransport func(*http.Request) (*http.Response, error)

func (fn embeddedSignupBudgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestEmbeddedSignupProviderPhasesShareDeadlineAndKeepAmbiguousState(t *testing.T) {
	for _, phase := range []string{"discovery", "registration", "subscription", "contact_sync", "history_sync"} {
		t.Run(phase, func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			meta.phoneIsOnBizApp = phase != "registration"
			if meta.phoneIsOnBizApp {
				meta.phonePlatformType = "SMB_CLOUD_API"
			}
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)
			var sharedDeadline time.Time
			syncRequests, blockedRequests, providerRequests := 0, 0, 0
			app.WhatsApp.HTTPClient.Transport = embeddedSignupBudgetTransport(func(request *http.Request) (*http.Response, error) {
				providerRequests++
				deadline, ok := request.Context().Deadline()
				require.True(t, ok, "every Meta phase must have a deadline")
				if sharedDeadline.IsZero() {
					sharedDeadline = deadline
				}
				assert.True(t, deadline.Equal(sharedDeadline), "later phases must not renew the operation budget: %s", request.URL.Path)
				if strings.HasSuffix(request.URL.Path, "/smb_app_data") {
					syncRequests++
				}
				blocked := phase == "discovery" && request.URL.Path == "/debug_token" ||
					phase == "registration" && strings.HasSuffix(request.URL.Path, "/register") ||
					phase == "subscription" && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/subscribed_apps") ||
					phase == "contact_sync" && syncRequests == 1 ||
					phase == "history_sync" && syncRequests == 2
				response, err := http.DefaultTransport.RoundTrip(request)
				if err != nil {
					return response, err
				}
				if blocked {
					// Meta accepted the request, but its acknowledgement is lost.
					// A timeout must preserve uncertainty, never trigger a replay.
					blockedRequests++
					_ = response.Body.Close()
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				if strings.HasSuffix(request.URL.Path, "/oauth/access_token") || request.URL.Path == "/debug_token" {
					// Consume budget in earlier serial phases before delaying the
					// selected phase; the remaining deadline must still be shared.
					timer := time.NewTimer(20 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-request.Context().Done():
						_ = response.Body.Close()
						return nil, request.Context().Err()
					}
				}
				return response, nil
			})
			mode := embeddedSignupModeCoexistence
			if !meta.phoneIsOnBizApp {
				mode = embeddedSignupModeClassic
			}
			req := testutil.NewJSONRequest(t, map[string]any{
				"code": "synthetic-delayed-code", "signup_mode": mode, "phone_id": phoneID, "waba_id": wabaID,
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
			started := time.Now()
			require.NoError(t, app.exchangeToken(req, 2*time.Second))
			assert.Less(t, time.Since(started), 4*time.Second, "provider phases must return within one budget plus local settlement")
			require.Equal(t, 1, blockedRequests)
			assert.GreaterOrEqual(t, providerRequests, 2)

			if phase == "discovery" {
				assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
				var count int64
				require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).Where("organization_id = ?", org.ID).Count(&count).Error)
				assert.Zero(t, count, "read-only deadline exhaustion must not claim a phone")
				return
			}
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			var account models.WhatsAppAccount
			require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&account).Error)
			assert.True(t, appcrypto.IsEncrypted(account.AccessToken))
			assert.Contains(t, string(testutil.GetResponseBody(req)), "warning")
			if phase == "registration" {
				assert.Equal(t, "pending_registration", account.Status)
				assert.True(t, appcrypto.IsEncrypted(account.Pin))
				assert.Equal(t, 1, meta.hit("/v21.0/"+phoneID+"/register"))
				assert.Zero(t, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
				return
			}
			assert.Equal(t, 1, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
			if phase == "subscription" {
				assert.Equal(t, "subscription_failed", account.Status)
				assert.Zero(t, syncRequests)
				return
			}
			assert.Equal(t, "active", account.Status)
			var state models.WhatsAppCoexistenceState
			require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account_id = ?", org.ID, account.ID).First(&state).Error)
			if phase == "contact_sync" {
				assert.Equal(t, 1, syncRequests, "deadline exhaustion must not start history")
				assert.Equal(t, models.CoexistenceSyncStatusRequesting, state.ContactSyncStatus)
				assert.Equal(t, "request_timeout", state.ContactSyncErrorCode)
				assert.Equal(t, models.CoexistenceSyncStatusPending, state.HistorySyncStatus)
				assert.Zero(t, state.HistorySyncAttempts)
			} else {
				assert.Equal(t, 2, syncRequests)
				assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
				assert.Equal(t, models.CoexistenceSyncStatusRequesting, state.HistorySyncStatus)
				assert.Equal(t, "request_timeout", state.HistorySyncErrorCode)
			}
			// An explicit later recovery may request the untouched history stage,
			// but cannot replay the ambiguous or already accepted one-time stage.
			app.WhatsApp.HTTPClient.Transport = http.DefaultTransport
			_, _, err := app.runCoexistenceSyncRequests(context.Background(), org.ID, account.ID, &whatsapp.Account{
				PhoneID: phoneID, BusinessID: wabaID, AccessToken: "synthetic-embedded-token", APIVersion: "v21.0",
			})
			require.NoError(t, err)
			assert.Equal(t, 2, meta.hit("/v21.0/"+phoneID+"/smb_app_data"), "recovery must never replay the ambiguous stage")
		})
	}
}

func TestEmbeddedSignupActiveSMBRefreshPreservesOneTimeSyncState(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-old-smb-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Stable Coexistence Clinic",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(existing).Error)
	onboardedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	deadline := onboardedAt.Add(models.CoexistenceSyncWindow)
	completedAt := onboardedAt.Add(time.Hour)
	state := &models.WhatsAppCoexistenceState{
		ID:                     uuid.New(),
		OrganizationID:         org.ID,
		WhatsAppAccountID:      existing.ID,
		BusinessPhoneNumber:    "+60123456789",
		OnboardingStatus:       models.CoexistenceOnboardingStatusReady,
		OnboardedAt:            &onboardedAt,
		SyncStatus:             models.CoexistenceSyncStatusCompleted,
		SyncStartedAt:          &onboardedAt,
		SyncCompletedAt:        &completedAt,
		SyncDeadlineAt:         &deadline,
		ContactSyncStatus:      models.CoexistenceSyncStatusCompleted,
		ContactSyncAttempts:    1,
		ContactSyncCompletedAt: &completedAt,
		HistoryConsent:         models.CoexistenceHistoryConsentGranted,
		HistorySyncStatus:      models.CoexistenceSyncStatusCompleted,
		HistorySyncAttempts:    1,
		HistoryProgressPercent: 100,
		HistoryCompletedAt:     &completedAt,
		LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata:      models.JSONB{},
		Version:                7,
	}
	require.NoError(t, app.DB.Create(state).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-active-smb-refresh-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/smb_app_data"),
		"an active token refresh must not replay Meta's one-time sync requests")

	var preserved models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("id = ?", state.ID).First(&preserved).Error)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, preserved.OnboardingStatus)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, preserved.SyncStatus)
	assert.Equal(t, 1, preserved.ContactSyncAttempts)
	assert.Equal(t, 1, preserved.HistorySyncAttempts)
	require.NotNil(t, preserved.SyncDeadlineAt)
	assert.Equal(t, deadline, preserved.SyncDeadlineAt.UTC())
}

func TestEmbeddedSignupActiveClassicAccountCanEnterCoexistence(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-classic-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	oldPIN, err := appcrypto.Encrypt("123456", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Classic Account Moving To Coexistence",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		Pin:            oldPIN,
		IsSMB:          false,
	}
	require.NoError(t, app.DB.Create(existing).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-active-classic-to-smb-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 0, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 2, meta.hit("/v21.0/"+phoneID+"/smb_app_data"))

	var stored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ?", existing.ID).First(&stored).Error)
	assert.True(t, stored.IsSMB)
	assert.Empty(t, stored.Pin)
	assert.Equal(t, "active", stored.Status)
	var state models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND whats_app_account_id = ?",
		org.ID,
		existing.ID,
	).First(&state).Error)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.HistorySyncStatus)
}

func TestEmbeddedSignupActiveExpiredSMBDoesNotReplayOneTimeSync(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-expired-smb-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Expired Coexistence Account",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(existing).Error)
	onboardedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	deadline := onboardedAt.Add(models.CoexistenceSyncWindow)
	state := &models.WhatsAppCoexistenceState{
		ID:                uuid.New(),
		OrganizationID:    org.ID,
		WhatsAppAccountID: existing.ID,
		OnboardingStatus:  models.CoexistenceOnboardingStatusExpired,
		OnboardedAt:       &onboardedAt,
		SyncStatus:        models.CoexistenceSyncStatusExpired,
		SyncStartedAt:     &onboardedAt,
		SyncDeadlineAt:    &deadline,
		ContactSyncStatus: models.CoexistenceSyncStatusExpired,
		HistoryConsent:    models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus: models.CoexistenceSyncStatusExpired,
		LifecycleStatus:   models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata: models.JSONB{},
		Version:           3,
	}
	require.NoError(t, app.DB.Create(state).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-expired-smb-refresh-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, 0, meta.hit("/v21.0/"+phoneID+"/smb_app_data"))

	var preserved models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("id = ?", state.ID).First(&preserved).Error)
	require.NotNil(t, preserved.OnboardedAt)
	assert.Equal(t, onboardedAt, preserved.OnboardedAt.UTC())
	assert.Equal(t, models.CoexistenceOnboardingStatusExpired, preserved.OnboardingStatus)
}

func TestEmbeddedSignupAfterOffboardAndReconnectStartsFreshCoexistenceCycle(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	meta.permanentToken = true
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-offboarded-smb-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Reonboarding Coexistence Account",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "disconnected",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(existing).Error)
	oldOnboardedAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	offboardedAt := oldOnboardedAt.Add(48 * time.Hour)
	reconnectedAt := offboardedAt.Add(time.Hour)
	oldCompletedAt := oldOnboardedAt.Add(time.Hour)
	oldDeadline := oldOnboardedAt.Add(models.CoexistenceSyncWindow)
	state := &models.WhatsAppCoexistenceState{
		ID:                     uuid.New(),
		OrganizationID:         org.ID,
		WhatsAppAccountID:      existing.ID,
		OnboardingStatus:       models.CoexistenceOnboardingStatusReady,
		OnboardedAt:            &oldOnboardedAt,
		SyncStatus:             models.CoexistenceSyncStatusCompleted,
		SyncStartedAt:          &oldOnboardedAt,
		SyncCompletedAt:        &oldCompletedAt,
		SyncDeadlineAt:         &oldDeadline,
		ContactSyncStatus:      models.CoexistenceSyncStatusCompleted,
		ContactSyncAttempts:    1,
		ContactSyncCompletedAt: &oldCompletedAt,
		HistoryConsent:         models.CoexistenceHistoryConsentGranted,
		HistorySyncStatus:      models.CoexistenceSyncStatusCompleted,
		HistorySyncAttempts:    1,
		HistoryProgressPercent: 100,
		HistoryCompletedAt:     &oldCompletedAt,
		LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
		LastLifecycleEvent:     "ACCOUNT_RECONNECTED",
		LastLifecycleEventAt:   &reconnectedAt,
		OffboardedAt:           &offboardedAt,
		ReconnectedAt:          &reconnectedAt,
		LifecycleMetadata:      models.JSONB{"event": "ACCOUNT_RECONNECTED"},
		Version:                5,
	}
	require.NoError(t, app.DB.Create(state).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-post-offboard-smb-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Equal(t, 1, meta.methodHits(http.MethodPost, "/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Equal(t, 2, meta.hit("/v21.0/"+phoneID+"/smb_app_data"))

	var reonboarded models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("id = ?", state.ID).First(&reonboarded).Error)
	require.NotNil(t, reonboarded.OnboardedAt)
	assert.True(t, reonboarded.OnboardedAt.After(reconnectedAt))
	assert.Equal(t, models.CoexistenceSyncStatusRequested, reonboarded.ContactSyncStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, reonboarded.HistorySyncStatus)
	assert.Equal(t, 1, reonboarded.ContactSyncAttempts)
	assert.Equal(t, 1, reonboarded.HistorySyncAttempts)
	assert.EqualValues(t, 2, reonboarded.OnboardingCycle)
	assert.Nil(t, reonboarded.OffboardedAt)
	assert.Equal(t, "EMBEDDED_SIGNUP_COMPLETED", reonboarded.LastLifecycleEvent)
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
		"code":        "synthetic-active-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
		"name":        "Must Not Replace Stable Name",
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
		"code":        "synthetic-unbound-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-config-race-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-rejected-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
		"name":        "Do Not Rename Rejected Clinic",
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
		"code":        "synthetic-timeout-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-soft-delete-reconnect-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
		"name":        "Restored Clinic Name",
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

func TestEmbeddedSignupSoftDeletedSMBRestoreDoesNotReopenOneTimeSyncCycle(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	oldToken, err := appcrypto.Encrypt("synthetic-deleted-smb-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Deleted Coexistence Clinic",
		PhoneID:        phoneID,
		BusinessID:     wabaID,
		AccessToken:    oldToken,
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, app.DB.Create(account).Error)
	onboardedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	deadline := onboardedAt.Add(models.CoexistenceSyncWindow)
	completedAt := onboardedAt.Add(time.Hour)
	state := &models.WhatsAppCoexistenceState{
		ID:                     uuid.New(),
		OrganizationID:         org.ID,
		WhatsAppAccountID:      account.ID,
		OnboardingStatus:       models.CoexistenceOnboardingStatusReady,
		OnboardedAt:            &onboardedAt,
		OnboardingCycle:        1,
		SyncStatus:             models.CoexistenceSyncStatusCompleted,
		SyncStartedAt:          &onboardedAt,
		SyncCompletedAt:        &completedAt,
		SyncDeadlineAt:         &deadline,
		ContactSyncStatus:      models.CoexistenceSyncStatusRequested,
		ContactSyncAttempts:    1,
		ContactSyncRequestedAt: &onboardedAt,
		HistoryConsent:         models.CoexistenceHistoryConsentGranted,
		HistorySyncStatus:      models.CoexistenceSyncStatusCompleted,
		HistorySyncAttempts:    1,
		HistoryProgressPercent: 100,
		HistoryCompletedAt:     &completedAt,
		LifecycleStatus:        models.CoexistenceLifecycleStatusConnected,
		LifecycleMetadata:      models.JSONB{},
		Version:                7,
	}
	require.NoError(t, app.DB.Create(state).Error)
	require.NoError(t, app.DB.Delete(account).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-soft-delete-smb-restore-code",
		"signup_mode": "coexistence",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
	require.NoError(t, app.ExchangeToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"))
	assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/smb_app_data"),
		"local soft deletion is not Meta offboarding and must not reopen one-time sync")

	var restored models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", account.ID, org.ID).First(&restored).Error)
	assert.Equal(t, "active", restored.Status)
	assert.True(t, restored.IsSMB)
	var preserved models.WhatsAppCoexistenceState
	require.NoError(t, app.DB.Where("id = ?", state.ID).First(&preserved).Error)
	assert.Equal(t, uint64(1), preserved.OnboardingCycle)
	assert.Equal(t, onboardedAt, preserved.OnboardedAt.UTC())
	assert.Equal(t, 1, preserved.ContactSyncAttempts)
	assert.Equal(t, 1, preserved.HistorySyncAttempts)
	assert.Equal(t, "EMBEDDED_SIGNUP_CREDENTIAL_REFRESH", preserved.LastLifecycleEvent)
	assert.Equal(t, models.CoexistenceLifecycleStatusConnected, preserved.LifecycleStatus)
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
		"code":        "synthetic-global-conflict-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
		"code":        "synthetic-alternate-member-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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

func TestEmbeddedSignupGeneratedNamesAcrossOrganizationsAndDiscovery(t *testing.T) {
	// Skip the entire contract when a disposable PostgreSQL test database is
	// unavailable; skipped subtests must not trip the final cross-org assertion.
	testutil.SetupTestDB(t)
	var generatedNames []string
	for _, testCase := range []struct {
		name         string
		provideIDs   bool
		explicitName string
	}{
		{name: "supplied IDs", provideIDs: true},
		{name: "discovered IDs"},
		{name: "discovered IDs with explicit name", explicitName: "Selected Clinic Account"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			phoneID, wabaID := contractGraphIDs()
			meta := newWhatsAppContractMeta(t, phoneID, wabaID)
			app := newWhatsAppContractApp(t, meta)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := contractWriter(t, app, org.ID)

			body := map[string]any{
				"code":        "synthetic-generated-name-code",
				"signup_mode": "classic",
			}
			if testCase.provideIDs {
				body["phone_id"] = phoneID
				body["waba_id"] = wabaID
			}
			if testCase.explicitName != "" {
				body["name"] = "  " + testCase.explicitName + "  "
			}
			req := testutil.NewJSONRequest(t, body)
			testutil.SetAuthContext(req, org.ID, user.ID)
			testutil.SetHeader(req, "X-Organization-ID", org.ID.String())
			require.NoError(t, app.ExchangeToken(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&stored).Error)
			if testCase.explicitName != "" {
				assert.Equal(t, testCase.explicitName, stored.Name)
				return
			}
			assert.Equal(t, "Synthetic Clinic (+60123456789) "+stored.ID.String(), stored.Name)
			generatedNames = append(generatedNames, stored.Name)
		})
	}
	require.Len(t, generatedNames, 2)
	assert.NotEqual(t, generatedNames[0], generatedNames[1])
}

func TestEmbeddedSignupSuperAdminCanPinExistingTargetOrganization(t *testing.T) {
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithSuperAdmin())

	req := testutil.NewJSONRequest(t, map[string]any{
		"code":        "synthetic-superadmin-target-code",
		"signup_mode": "classic",
		"phone_id":    phoneID,
		"waba_id":     wabaID,
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
				"code":        "must-not-exchange-deleted-target",
				"signup_mode": "classic",
				"phone_id":    phoneID,
				"waba_id":     wabaID,
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
