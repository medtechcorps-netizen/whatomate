package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// The server default has advanced past the version stored on a live account.
const (
	coexistenceStoredAPIVersion     = "v21.0"
	coexistenceConfiguredAPIVersion = "v24.0"
)

type coexistenceVersionFixture struct {
	app     *App
	meta    *whatsappContractMeta
	org     *models.Organization
	user    *models.User
	phoneID string
	wabaID  string
}

func newCoexistenceVersionFixture(t *testing.T) *coexistenceVersionFixture {
	t.Helper()
	phoneID, wabaID := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	meta.phoneIsOnBizApp = true
	meta.phonePlatformType = "SMB_CLOUD_API"
	app := newWhatsAppContractApp(t, meta)
	app.Config.WhatsApp.APIVersion = coexistenceConfiguredAPIVersion
	org := testutil.CreateTestOrganization(t, app.DB)
	return &coexistenceVersionFixture{
		app:     app,
		meta:    meta,
		org:     org,
		user:    contractWriter(t, app, org.ID),
		phoneID: phoneID,
		wabaID:  wabaID,
	}
}

// liveClassicAccount stores an active, non-SMB account that has been operating
// on the older stored API version.
func (f *coexistenceVersionFixture) liveClassicAccount(t *testing.T, apiVersion string) models.WhatsAppAccount {
	t.Helper()
	token, err := appcrypto.Encrypt("synthetic-original-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	pin, err := appcrypto.Encrypt("123456", integrationTestEncryptionKey)
	require.NoError(t, err)
	return models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         f.org.ID,
		Name:                   "Existing conversations " + uuid.NewString(),
		PhoneID:                f.phoneID,
		BusinessID:             f.wabaID,
		AccessToken:            token,
		Pin:                    pin,
		APIVersion:             apiVersion,
		Status:                 "active",
		IsDefaultIncoming:      true,
		IsDefaultOutgoing:      true,
		AutoReadReceipt:        true,
		BusinessCallingEnabled: true,
	}
}

func (f *coexistenceVersionFixture) exchange(t *testing.T, orgID, userID uuid.UUID, body map[string]any) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetHeader(req, "X-Organization-ID", orgID.String())
	require.NoError(t, f.app.ExchangeToken(req))
	return req
}

func (f *coexistenceVersionFixture) signupBody(code, mode string) map[string]any {
	return map[string]any{
		"code":        code,
		"signup_mode": mode,
		"phone_id":    f.phoneID,
		"waba_id":     f.wabaID,
	}
}

func (f *coexistenceVersionFixture) reload(t *testing.T, accountID uuid.UUID) models.WhatsAppAccount {
	t.Helper()
	var stored models.WhatsAppAccount
	require.NoError(t, f.app.DB.Unscoped().Where("id = ?", accountID).First(&stored).Error)
	return stored
}

func (f *coexistenceVersionFixture) phoneRows(t *testing.T) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.app.DB.Unscoped().Model(&models.WhatsAppAccount{}).
		Where("BTRIM(phone_id) = ?", f.phoneID).
		Count(&count).Error)
	return count
}

func (f *coexistenceVersionFixture) coexistenceStates(t *testing.T) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.app.DB.Model(&models.WhatsAppCoexistenceState{}).
		Where("organization_id = ?", f.org.ID).
		Count(&count).Error)
	return count
}

// assertNoProviderMutation proves no Meta registration, subscription write or
// one-time Coexistence sync request ran on any API version.
func (f *coexistenceVersionFixture) assertNoProviderMutation(t *testing.T) {
	t.Helper()
	for _, version := range []string{coexistenceStoredAPIVersion, "v22.0", coexistenceConfiguredAPIVersion, "v25.0"} {
		assert.Zero(t, f.meta.hit("/"+version+"/"+f.phoneID+"/register"), version)
		assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+version+"/"+f.wabaID+"/subscribed_apps"), version)
		assert.Zero(t, f.meta.hit("/"+version+"/"+f.phoneID+"/smb_app_data"), version)
	}
}

// versionHits counts every Graph request made under one API version prefix.
func (m *whatsappContractMeta) versionHits(version string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int
	for key, count := range m.hits {
		if strings.HasPrefix(key, "/"+version+"/") {
			total += count
		}
	}
	return total
}

func TestEmbeddedSignupCoexistenceKeepsLiveAccountAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		omitIDs     bool
		failure     string
		wantStatus  int
		wantMessage string
		wantVersion string
	}{
		{name: "explicit identity", wantStatus: fasthttp.StatusOK, wantVersion: coexistenceStoredAPIVersion},
		{name: "token discovered identity", omitIDs: true, wantStatus: fasthttp.StatusOK, wantVersion: coexistenceStoredAPIVersion},
		{name: "another WABA is rejected", failure: "waba", wantStatus: fasthttp.StatusConflict, wantMessage: "already connected", wantVersion: coexistenceConfiguredAPIVersion},
		{name: "another app is rejected", failure: "app", wantStatus: fasthttp.StatusConflict, wantMessage: "already connected", wantVersion: coexistenceConfiguredAPIVersion},
		{name: "another tenant is rejected", failure: "tenant", wantStatus: fasthttp.StatusConflict, wantMessage: "already connected", wantVersion: coexistenceConfiguredAPIVersion},
		{name: "missing app subscription is rejected", failure: "subscription", wantStatus: fasthttp.StatusConflict, wantMessage: "not subscribed", wantVersion: coexistenceStoredAPIVersion},
		{name: "missing token grant is rejected", failure: "grant", wantStatus: fasthttp.StatusBadRequest, wantMessage: "not granted"},
		{name: "phone membership must match", failure: "membership", wantStatus: fasthttp.StatusBadRequest, wantMessage: "does not belong", wantVersion: coexistenceStoredAPIVersion},
		{name: "provider must confirm coexistence", failure: "provider-mode", wantStatus: fasthttp.StatusBadRequest, wantMessage: "does not match", wantVersion: coexistenceStoredAPIVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoexistenceVersionFixture(t)
			f.meta.tokensByCode = map[string]string{"synthetic-later-refresh": "synthetic-refreshed-token"}
			account := f.liveClassicAccount(t, coexistenceStoredAPIVersion)
			var err error
			switch tc.failure {
			case "waba":
				account.BusinessID = testutil.NewTestGraphObjectID()
			case "app":
				account.AppID = testutil.NewTestGraphObjectID()
				account.AppSecret, err = appcrypto.Encrypt("synthetic-other-app-secret", integrationTestEncryptionKey)
				require.NoError(t, err)
			case "subscription":
				f.meta.subscribedAppIDs = nil
			case "grant":
				f.meta.granularTargetIDs = []string{testutil.NewTestGraphObjectID()}
			case "membership":
				f.meta.listedPhoneID = testutil.NewTestGraphObjectID()
			case "provider-mode":
				f.meta.phoneIsOnBizApp = false
				f.meta.phonePlatformType = "CLOUD_API"
			}
			require.NoError(t, f.app.DB.Create(&account).Error)
			requestOrgID, userID := f.org.ID, f.user.ID
			if tc.failure == "tenant" {
				requestOrgID = testutil.CreateTestOrganization(t, f.app.DB).ID
				userID = contractWriter(t, f.app, requestOrgID).ID
			}
			body := f.signupBody("synthetic-coexistence-version-code", embeddedSignupModeCoexistence)
			body["name"] = "Do not rename"
			if tc.omitIDs {
				delete(body, "phone_id")
				delete(body, "waba_id")
			}
			req := f.exchange(t, requestOrgID, userID, body)
			if tc.wantStatus == fasthttp.StatusOK {
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(req.RequestCtx.Response.Body()))
			} else {
				testutil.AssertErrorResponse(t, req, tc.wantStatus, tc.wantMessage)
			}

			stored := f.reload(t, account.ID)
			assert.Equal(t, account.OrganizationID, stored.OrganizationID)
			assert.Equal(t, account.Name, stored.Name)
			assert.Equal(t, coexistenceStoredAPIVersion, stored.APIVersion, "the stored version is never migrated")
			assert.Equal(t, account.PhoneID, stored.PhoneID)
			assert.Equal(t, account.BusinessID, stored.BusinessID)
			assert.Equal(t, "active", stored.Status)
			assert.True(t, stored.IsDefaultIncoming && stored.IsDefaultOutgoing && stored.AutoReadReceipt && stored.BusinessCallingEnabled)
			assert.EqualValues(t, 1, f.phoneRows(t), "no second account row may be created for the phone")
			for _, version := range []string{coexistenceStoredAPIVersion, coexistenceConfiguredAPIVersion} {
				assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+version+"/"+f.phoneID+"/register"))
				assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+version+"/"+f.wabaID+"/subscribed_apps"))
			}
			if tc.wantVersion != "" {
				assert.Positive(t, f.meta.methodHits(http.MethodGet, "/"+tc.wantVersion+"/"+f.phoneID))
				assert.Positive(t, f.meta.methodHits(http.MethodGet, "/"+tc.wantVersion+"/"+f.wabaID+"/phone_numbers"))
			}
			// The authorization code is always exchanged on the configured version.
			assert.Equal(t, 1, f.meta.methodHits(http.MethodPost, "/"+coexistenceConfiguredAPIVersion+"/oauth/access_token"))
			assert.Zero(t, f.meta.hit("/"+coexistenceStoredAPIVersion+"/oauth/access_token"))
			if tc.wantStatus != fasthttp.StatusOK {
				assert.False(t, stored.IsSMB)
				assert.Equal(t, account.AccessToken, stored.AccessToken)
				assert.Equal(t, account.Pin, stored.Pin)
				assert.Zero(t, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
				assert.Zero(t, f.meta.hit("/"+coexistenceConfiguredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
				assert.Zero(t, f.coexistenceStates(t))
				return
			}

			assert.True(t, stored.IsSMB)
			assert.Empty(t, stored.Pin)
			assert.NotEqual(t, account.AccessToken, stored.AccessToken)
			assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/debug_token"))
			assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/"+coexistenceStoredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
			assert.Equal(t, 2, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
			if tc.omitIDs {
				// Phone discovery precedes knowing which account is involved.
				assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/"+coexistenceConfiguredAPIVersion+"/"+f.wabaID+"/phone_numbers"))
				assert.Equal(t, 2, f.meta.versionHits(coexistenceConfiguredAPIVersion))
			} else {
				assert.Equal(t, 1, f.meta.versionHits(coexistenceConfiguredAPIVersion), "only the code exchange uses the configured version")
			}
			var state models.WhatsAppCoexistenceState
			require.NoError(t, f.app.DB.Where("whats_app_account_id = ?", account.ID).First(&state).Error)
			assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
			assert.Equal(t, models.CoexistenceSyncStatusRequested, state.HistorySyncStatus)

			// A later credential refresh keeps using the preserved version without
			// treating the already-converted SMB row as fresh onboarding.
			refresh := f.exchange(t, requestOrgID, userID, f.signupBody("synthetic-later-refresh", embeddedSignupModeCoexistence))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refresh), string(refresh.RequestCtx.Response.Body()))
			stored = f.reload(t, account.ID)
			assert.Equal(t, coexistenceStoredAPIVersion, stored.APIVersion)
			assert.True(t, stored.IsSMB)
			refreshedToken, err := appcrypto.Decrypt(stored.AccessToken, integrationTestEncryptionKey)
			require.NoError(t, err)
			assert.Equal(t, "synthetic-refreshed-token", refreshedToken)
			var refreshedState models.WhatsAppCoexistenceState
			require.NoError(t, f.app.DB.Where("id = ?", state.ID).First(&refreshedState).Error)
			assert.Equal(t, state.OnboardingCycle, refreshedState.OnboardingCycle)
			assert.Equal(t, state.OnboardedAt, refreshedState.OnboardedAt)
			assert.Equal(t, state.ContactSyncAttempts, refreshedState.ContactSyncAttempts)
			assert.Equal(t, state.HistorySyncAttempts, refreshedState.HistorySyncAttempts)
			assert.Equal(t, state.ContactSyncStatus, refreshedState.ContactSyncStatus)
			assert.Equal(t, state.HistorySyncStatus, refreshedState.HistorySyncStatus)
			assert.Equal(t, 2, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
			assert.Equal(t, 2, f.meta.methodHits(http.MethodGet, "/"+coexistenceStoredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
			assert.Zero(t, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+f.phoneID+"/register"))
			assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+coexistenceStoredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
			assert.EqualValues(t, 1, f.phoneRows(t))
		})
	}
}

// Every change below lands after the version was chosen and the fresh token
// was validated against it, but before the locked claim. Each must abort the
// conversion; none may fall through to a new account or a registration.
func TestEmbeddedSignupCoexistenceKeptAPIVersionFencesConcurrentChanges(t *testing.T) {
	type change struct {
		name        string
		wantMessage string
		apply       func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error
		// wantLiveRows is the number of live rows for the phone afterwards.
		wantLiveRows int64
		// removed marks a change that hard-deletes the inspected row.
		removed    bool
		wantStored func(t *testing.T, stored models.WhatsAppAccount)
	}
	const superseded = "WhatsApp connection changed; reload and retry"
	for _, tc := range []change{
		{
			name:        "concurrent edit",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).
					Update("name", "Renamed during signup "+uuid.NewString()).Error
			},
			wantLiveRows: 1,
		},
		{
			name:        "api version changed in place",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Exec("UPDATE whatsapp_accounts SET api_version = ? WHERE id = ?", "v22.0", account.ID).Error
			},
			wantLiveRows: 1,
			wantStored: func(t *testing.T, stored models.WhatsAppAccount) {
				assert.Equal(t, "v22.0", stored.APIVersion)
			},
		},
		{
			name:        "status changed in place",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Exec("UPDATE whatsapp_accounts SET status = ? WHERE id = ?", "disconnected", account.ID).Error
			},
			wantLiveRows: 1,
			wantStored: func(t *testing.T, stored models.WhatsAppAccount) {
				assert.Equal(t, "disconnected", stored.Status)
			},
		},
		{
			name:        "provider classification changed in place",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Exec("UPDATE whatsapp_accounts SET is_smb = TRUE WHERE id = ?", account.ID).Error
			},
			wantLiveRows: 1,
			wantStored: func(t *testing.T, stored models.WhatsAppAccount) {
				assert.True(t, stored.IsSMB)
			},
		},
		{
			name:        "account soft deleted",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Exec("UPDATE whatsapp_accounts SET deleted_at = NOW() WHERE id = ?", account.ID).Error
			},
			wantLiveRows: 0,
			wantStored: func(t *testing.T, stored models.WhatsAppAccount) {
				assert.True(t, stored.DeletedAt.Valid)
			},
		},
		{
			name:        "account replaced by another row",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				if err := f.app.DB.Exec("UPDATE whatsapp_accounts SET deleted_at = NOW() WHERE id = ?", account.ID).Error; err != nil {
					return err
				}
				replacement := models.WhatsAppAccount{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: account.OrganizationID,
					Name:           "Replacement " + uuid.NewString(),
					PhoneID:        account.PhoneID,
					BusinessID:     account.BusinessID,
					AccessToken:    account.AccessToken,
					APIVersion:     account.APIVersion,
					Status:         "active",
				}
				if err := f.app.DB.Create(&replacement).Error; err != nil {
					return err
				}
				// Give the replacement the inspected row's timestamp so only the
				// account identity distinguishes it.
				return f.app.DB.Exec(
					"UPDATE whatsapp_accounts SET updated_at = (SELECT updated_at FROM whatsapp_accounts WHERE id = ?) WHERE id = ?",
					account.ID,
					replacement.ID,
				).Error
			},
			wantLiveRows: 1,
			wantStored: func(t *testing.T, stored models.WhatsAppAccount) {
				assert.True(t, stored.DeletedAt.Valid)
			},
		},
		{
			name:        "account hard deleted",
			wantMessage: superseded,
			apply: func(f *coexistenceVersionFixture, account *models.WhatsAppAccount) error {
				return f.app.DB.Exec("DELETE FROM whatsapp_accounts WHERE id = ?", account.ID).Error
			},
			wantLiveRows: 0,
			removed:      true,
		},
		{
			name:        "server configuration changed",
			wantMessage: "Meta integration settings changed",
			apply: func(f *coexistenceVersionFixture, _ *models.WhatsAppAccount) error {
				f.app.Config.WhatsApp.APIVersion = "v25.0"
				return nil
			},
			wantLiveRows: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoexistenceVersionFixture(t)
			account := f.liveClassicAccount(t, coexistenceStoredAPIVersion)
			require.NoError(t, f.app.DB.Create(&account).Error)
			// The read-only subscription proof is the last provider call before
			// the locked claim and already uses the kept version.
			applied := make(chan error, 1)
			f.meta.onSubscriptionRead = func() { applied <- tc.apply(f, &account) }

			req := f.exchange(t, f.org.ID, f.user.ID, f.signupBody("synthetic-version-race-code", embeddedSignupModeCoexistence))
			require.Len(t, applied, 1, "the change must land during the subscription proof")
			require.NoError(t, <-applied)
			testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, tc.wantMessage)

			if tc.removed {
				assert.Zero(t, f.phoneRows(t), "no account may be recreated for the phone")
			} else {
				stored := f.reload(t, account.ID)
				assert.Equal(t, account.AccessToken, stored.AccessToken, "the stored credentials must not be replaced")
				assert.Equal(t, account.Pin, stored.Pin)
				assert.Equal(t, account.BusinessID, stored.BusinessID)
				if tc.wantStored != nil {
					tc.wantStored(t, stored)
				} else {
					assert.Equal(t, coexistenceStoredAPIVersion, stored.APIVersion)
					assert.Equal(t, "active", stored.Status)
					assert.False(t, stored.IsSMB)
					assert.False(t, stored.DeletedAt.Valid)
				}
			}
			var liveRows []models.WhatsAppAccount
			require.NoError(t, f.app.DB.Where("BTRIM(phone_id) = ?", f.phoneID).Find(&liveRows).Error)
			require.Len(t, liveRows, int(tc.wantLiveRows))
			for _, live := range liveRows {
				assert.Equal(t, account.AccessToken, live.AccessToken, "no live row may receive the new token")
				assert.NotEqual(t, "pending_registration", live.Status)
				assert.NotEqual(t, "pending_subscription", live.Status)
			}
			assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/"+coexistenceStoredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
			assert.Zero(t, f.coexistenceStates(t))
			f.assertNoProviderMutation(t)
		})
	}
}

func TestEmbeddedSignupAccountAPIVersionKeepsOnlyExactLiveAccount(t *testing.T) {
	type expectation int
	const (
		configured expectation = iota
		kept
	)
	for _, tc := range []struct {
		name     string
		mode     string
		noRow    bool
		otherOrg bool
		prepare  func(t *testing.T, account *models.WhatsAppAccount)
		after    func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount)
		want     expectation
		wantVer  string
	}{
		{name: "exact live classic account", mode: embeddedSignupModeCoexistence, want: kept, wantVer: coexistenceStoredAPIVersion},
		{
			name: "exact live coexistence account", mode: embeddedSignupModeCoexistence, want: kept, wantVer: coexistenceStoredAPIVersion,
			prepare: func(_ *testing.T, account *models.WhatsAppAccount) { account.IsSMB = true; account.Pin = "" },
		},
		{
			name: "stored version newer than configured", mode: embeddedSignupModeCoexistence, want: kept, wantVer: "v25.0",
			prepare: func(_ *testing.T, account *models.WhatsAppAccount) { account.APIVersion = "v25.0" },
		},
		{
			name: "matching legacy app credentials", mode: embeddedSignupModeCoexistence, want: kept, wantVer: coexistenceStoredAPIVersion,
			prepare: func(t *testing.T, account *models.WhatsAppAccount) {
				var err error
				account.AppID = contractMetaAppID
				account.AppSecret, err = appcrypto.Encrypt(contractMetaAppSecret, integrationTestEncryptionKey)
				require.NoError(t, err)
			},
		},
		{name: "classic signup", mode: embeddedSignupModeClassic, want: configured},
		{name: "no account for the phone", mode: embeddedSignupModeCoexistence, noRow: true, want: configured},
		{
			name: "stored version equals configured", mode: embeddedSignupModeCoexistence, want: configured,
			prepare: func(_ *testing.T, account *models.WhatsAppAccount) {
				account.APIVersion = coexistenceConfiguredAPIVersion
			},
		},
		{
			name: "padded stored version equals configured", mode: embeddedSignupModeCoexistence, want: configured,
			after: func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount) {
				require.NoError(t, f.app.DB.Exec("UPDATE whatsapp_accounts SET api_version = ? WHERE id = ?", " "+coexistenceConfiguredAPIVersion+" ", account.ID).Error)
			},
		},
		{
			name: "empty stored version", mode: embeddedSignupModeCoexistence, want: configured,
			after: func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount) {
				require.NoError(t, f.app.DB.Exec("UPDATE whatsapp_accounts SET api_version = '' WHERE id = ?", account.ID).Error)
			},
		},
		{
			name: "blank stored version", mode: embeddedSignupModeCoexistence, want: configured,
			after: func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount) {
				require.NoError(t, f.app.DB.Exec("UPDATE whatsapp_accounts SET api_version = '   ' WHERE id = ?", account.ID).Error)
			},
		},
		{
			name: "account is not active", mode: embeddedSignupModeCoexistence, want: configured,
			prepare: func(_ *testing.T, account *models.WhatsAppAccount) { account.Status = "disconnected" },
		},
		{
			name: "account is soft deleted", mode: embeddedSignupModeCoexistence, want: configured,
			after: func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount) {
				require.NoError(t, f.app.DB.Delete(&models.WhatsAppAccount{}, "id = ?", account.ID).Error)
			},
		},
		{
			name: "account belongs to another WABA", mode: embeddedSignupModeCoexistence, want: configured,
			prepare: func(_ *testing.T, account *models.WhatsAppAccount) {
				account.BusinessID = testutil.NewTestGraphObjectID()
			},
		},
		{
			name: "account pins another app", mode: embeddedSignupModeCoexistence, want: configured,
			prepare: func(t *testing.T, account *models.WhatsAppAccount) {
				var err error
				account.AppID = testutil.NewTestGraphObjectID()
				account.AppSecret, err = appcrypto.Encrypt("synthetic-other-app-secret", integrationTestEncryptionKey)
				require.NoError(t, err)
			},
		},
		{name: "account belongs to another organization", mode: embeddedSignupModeCoexistence, otherOrg: true, want: configured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoexistenceVersionFixture(t)
			account := f.liveClassicAccount(t, coexistenceStoredAPIVersion)
			if tc.prepare != nil {
				tc.prepare(t, &account)
			}
			if !tc.noRow {
				require.NoError(t, f.app.DB.Create(&account).Error)
			}
			if tc.after != nil {
				tc.after(t, f, &account)
			}
			orgID := f.org.ID
			if tc.otherOrg {
				orgID = testutil.CreateTestOrganization(t, f.app.DB).ID
			}
			snapshot := embeddedSignupMetaSnapshot{
				appID:      contractMetaAppID,
				appSecret:  contractMetaAppSecret,
				apiVersion: coexistenceConfiguredAPIVersion,
			}

			version, err := f.app.embeddedSignupAccountAPIVersion(orgID, tc.mode, " "+f.phoneID+" ", f.wabaID, snapshot)
			require.NoError(t, err)
			if tc.want == configured {
				assert.Equal(t, embeddedSignupAccountVersion{apiVersion: coexistenceConfiguredAPIVersion}, version)
				return
			}
			stored := f.reload(t, account.ID)
			assert.Equal(t, tc.wantVer, version.apiVersion)
			assert.Equal(t, account.ID, version.accountID)
			assert.True(t, stored.UpdatedAt.Equal(version.updatedAt))
			assert.Equal(t, stored.IsSMB, version.isSMB)
		})
	}
}

func TestEmbeddedSignupUsesConfiguredAPIVersionOutsideKeptLiveAccount(t *testing.T) {
	t.Run("new coexistence account", func(t *testing.T) {
		f := newCoexistenceVersionFixture(t)
		req := f.exchange(t, f.org.ID, f.user.ID, f.signupBody("synthetic-new-coexistence-code", embeddedSignupModeCoexistence))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(req.RequestCtx.Response.Body()))

		var stored models.WhatsAppAccount
		require.NoError(t, f.app.DB.Where("organization_id = ? AND phone_id = ?", f.org.ID, f.phoneID).First(&stored).Error)
		assert.Equal(t, coexistenceConfiguredAPIVersion, stored.APIVersion)
		assert.True(t, stored.IsSMB)
		assert.Equal(t, "active", stored.Status)
		assert.Equal(t, 1, f.meta.methodHits(http.MethodPost, "/"+coexistenceConfiguredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
		assert.Equal(t, 2, f.meta.hit("/"+coexistenceConfiguredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
		assert.Zero(t, f.meta.versionHits(coexistenceStoredAPIVersion))
	})

	t.Run("new classic account", func(t *testing.T) {
		f := newCoexistenceVersionFixture(t)
		f.meta.phoneIsOnBizApp = false
		f.meta.phonePlatformType = "CLOUD_API"
		req := f.exchange(t, f.org.ID, f.user.ID, f.signupBody("synthetic-new-classic-code", embeddedSignupModeClassic))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(req.RequestCtx.Response.Body()))

		var stored models.WhatsAppAccount
		require.NoError(t, f.app.DB.Where("organization_id = ? AND phone_id = ?", f.org.ID, f.phoneID).First(&stored).Error)
		assert.Equal(t, coexistenceConfiguredAPIVersion, stored.APIVersion)
		assert.False(t, stored.IsSMB)
		assert.Equal(t, "active", stored.Status)
		assert.Equal(t, 1, f.meta.methodHits(http.MethodPost, "/"+coexistenceConfiguredAPIVersion+"/"+f.phoneID+"/register"))
		assert.Equal(t, 1, f.meta.methodHits(http.MethodPost, "/"+coexistenceConfiguredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
		assert.Zero(t, f.meta.versionHits(coexistenceStoredAPIVersion))
	})

	for _, tc := range []struct {
		name    string
		mode    string
		prepare func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount)
	}{
		{
			name: "classic reconnect of a live account on another version",
			mode: embeddedSignupModeClassic,
		},
		{
			name: "coexistence reconnect of a live account without a stored version",
			mode: embeddedSignupModeCoexistence,
			prepare: func(t *testing.T, f *coexistenceVersionFixture, account *models.WhatsAppAccount) {
				require.NoError(t, f.app.DB.Exec("UPDATE whatsapp_accounts SET api_version = '' WHERE id = ?", account.ID).Error)
				account.APIVersion = ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoexistenceVersionFixture(t)
			if tc.mode == embeddedSignupModeClassic {
				f.meta.phoneIsOnBizApp = false
				f.meta.phonePlatformType = "CLOUD_API"
			}
			account := f.liveClassicAccount(t, coexistenceStoredAPIVersion)
			require.NoError(t, f.app.DB.Create(&account).Error)
			if tc.prepare != nil {
				tc.prepare(t, f, &account)
			}
			before := f.reload(t, account.ID)

			req := f.exchange(t, f.org.ID, f.user.ID, f.signupBody("synthetic-unchanged-contract-code", tc.mode))
			testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
			stored := f.reload(t, account.ID)
			assert.Equal(t, before.APIVersion, stored.APIVersion)
			assert.Equal(t, before.AccessToken, stored.AccessToken)
			assert.Equal(t, before.Pin, stored.Pin)
			assert.Equal(t, "active", stored.Status)
			assert.False(t, stored.IsSMB)
			assert.True(t, stored.UpdatedAt.Equal(before.UpdatedAt))
			assert.Zero(t, f.meta.versionHits(coexistenceStoredAPIVersion), "the stored version is not used outside the kept Coexistence path")
			assert.Positive(t, f.meta.methodHits(http.MethodGet, "/"+coexistenceConfiguredAPIVersion+"/"+f.phoneID))
			assert.Zero(t, f.coexistenceStates(t))
			f.assertNoProviderMutation(t)
		})
	}

	t.Run("coexistence reconnect when stored version equals configured", func(t *testing.T) {
		f := newCoexistenceVersionFixture(t)
		account := f.liveClassicAccount(t, coexistenceConfiguredAPIVersion)
		require.NoError(t, f.app.DB.Create(&account).Error)

		req := f.exchange(t, f.org.ID, f.user.ID, f.signupBody("synthetic-same-version-code", embeddedSignupModeCoexistence))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(req.RequestCtx.Response.Body()))
		stored := f.reload(t, account.ID)
		assert.Equal(t, coexistenceConfiguredAPIVersion, stored.APIVersion)
		assert.True(t, stored.IsSMB)
		assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/"+coexistenceConfiguredAPIVersion+"/"+f.wabaID+"/subscribed_apps"))
		assert.Equal(t, 2, f.meta.hit("/"+coexistenceConfiguredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
		assert.Zero(t, f.meta.versionHits(coexistenceStoredAPIVersion))
	})
}
