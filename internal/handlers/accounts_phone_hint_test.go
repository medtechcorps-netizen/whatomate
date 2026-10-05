package handlers

import (
	"net/http"
	"strings"
	"testing"

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

// Synthetic numbers only. The fixture's own phone is listed and reported by
// Meta as +60123456789; decoys are other numbers listed under the same WABA.
const (
	phoneHintSelectedDigits = "60123456789"
	phoneHintDecoyDisplay   = "+60 19-876 5432"
	phoneHintDecoyDigits    = "60198765432"
)

func TestNormalizeEmbeddedSignupPhoneNumberHint(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "", want: ""},
		{raw: "   ", want: ""},
		{raw: "60123456789", want: "60123456789"},
		{raw: "+60123456789", want: "60123456789"},
		{raw: " +60 12-345 6789 ", want: "60123456789"},
		{raw: "+1 (631) 555.0100", want: "16315550100"},
		{raw: "+6831234", want: "6831234"},
		{raw: "+123456789012345", want: "123456789012345"},
		{raw: "012-345 6789", wantErr: true},               // local number without country code
		{raw: "0060123456789", wantErr: true},              // international dialling prefix
		{raw: "+1234567890123456", wantErr: true},          // 16 digits
		{raw: "+60 1234", wantErr: true},                   // 6 digits
		{raw: "++60123456789", wantErr: true},              // second plus
		{raw: "60+123456789", wantErr: true},               // plus inside the number
		{raw: "+6O123456789", wantErr: true},               // letter O
		{raw: "+60123456789 ext 2", wantErr: true},         // extension text
		{raw: "+60/123456789", wantErr: true},              // unsupported separator
		{raw: "６０123456789", wantErr: true},                // full-width digits
		{raw: strings.Repeat("1 ", 17), wantErr: true},     // longer than 32 characters
		{raw: "+60_123456789", wantErr: true},              // underscore
		{raw: "+60\t123456789", wantErr: true},             // tab inside the number
		{raw: "tel:+60123456789", wantErr: true},           // URI scheme
		{raw: "+60 12 345 6789; DROP", wantErr: true},      // trailing text
		{raw: "+60-12-345-6789", want: "60123456789"},      // hyphenated
		{raw: "(60) 12 345 6789", want: "60123456789"},     // parentheses
		{raw: "+60 (0) 12 345 6789", want: "600123456789"}, // trunk zero kept as typed; it then matches nothing
	} {
		got, err := normalizeEmbeddedSignupPhoneNumberHint(tc.raw)
		if tc.wantErr {
			require.ErrorIs(t, err, errEmbeddedSignupPhoneNumberHintInvalid, "%q", tc.raw)
			assert.Empty(t, got, "%q", tc.raw)
			continue
		}
		require.NoError(t, err, "%q", tc.raw)
		assert.Equal(t, tc.want, got, "%q", tc.raw)
	}
}

func TestSelectEmbeddedSignupDiscoveredPhone(t *testing.T) {
	selected := whatsapp.WABAPhoneNumber{ID: "110000000000901", DisplayPhoneNumber: "+60 12-345 6789"}
	decoy := whatsapp.WABAPhoneNumber{ID: "110000000000902", DisplayPhoneNumber: phoneHintDecoyDisplay}
	duplicate := whatsapp.WABAPhoneNumber{ID: "110000000000903", DisplayPhoneNumber: "60123456789"}
	unnumbered := whatsapp.WABAPhoneNumber{ID: "110000000000904"}

	for _, tc := range []struct {
		name        string
		phones      []whatsapp.WABAPhoneNumber
		mode        string
		hint        string
		wantID      string
		wantMessage string
	}{
		{name: "single phone without hint", phones: []whatsapp.WABAPhoneNumber{decoy}, mode: embeddedSignupModeCoexistence, wantID: decoy.ID},
		{name: "single phone with matching hint", phones: []whatsapp.WABAPhoneNumber{selected}, mode: embeddedSignupModeCoexistence, hint: phoneHintSelectedDigits, wantID: selected.ID},
		{name: "single phone with another number", phones: []whatsapp.WABAPhoneNumber{decoy}, mode: embeddedSignupModeCoexistence, hint: phoneHintSelectedDigits, wantMessage: "the number ending in 6789 is not listed"},
		{name: "several phones with matching hint", phones: []whatsapp.WABAPhoneNumber{decoy, unnumbered, selected}, mode: embeddedSignupModeCoexistence, hint: phoneHintSelectedDigits, wantID: selected.ID},
		{name: "several phones with unmatched hint", phones: []whatsapp.WABAPhoneNumber{decoy, unnumbered}, mode: embeddedSignupModeCoexistence, hint: phoneHintSelectedDigits, wantMessage: "the number ending in 6789 is not listed"},
		{name: "several phones match the hint", phones: []whatsapp.WABAPhoneNumber{selected, decoy, duplicate}, mode: embeddedSignupModeCoexistence, hint: phoneHintSelectedDigits, wantMessage: "more than one phone in the selected WhatsApp Business Account matches the number ending in 6789"},
		{name: "several phones without hint in Coexistence", phones: []whatsapp.WABAPhoneNumber{selected, decoy}, mode: embeddedSignupModeCoexistence, wantMessage: "enter the WhatsApp Business app number you are connecting"},
		{name: "several phones in classic", phones: []whatsapp.WABAPhoneNumber{selected, decoy}, mode: embeddedSignupModeClassic, wantMessage: "reconnect and select exactly one phone number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phone, err := selectEmbeddedSignupDiscoveredPhone(tc.phones, tc.mode, tc.hint)
			if tc.wantMessage == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.wantID, phone.ID)
				return
			}
			require.ErrorContains(t, err, tc.wantMessage)
			assert.Empty(t, phone.ID)
			assert.True(t, strings.HasPrefix(err.Error(), "the number") ||
				strings.HasPrefix(err.Error(), "more than one") ||
				strings.HasPrefix(err.Error(), "multiple phone numbers"))
			// Only the last four typed digits may appear, never another number.
			assert.NotContains(t, err.Error(), phoneHintSelectedDigits)
			assert.NotContains(t, err.Error(), "12-345")
			assert.NotContains(t, err.Error(), phoneHintDecoyDigits)
			assert.NotContains(t, err.Error(), phoneHintDecoyDisplay)
		})
	}
}

type phoneHintFixture struct {
	app     *App
	meta    *whatsappContractMeta
	org     *models.Organization
	user    *models.User
	phoneID string
	wabaID  string
	decoyID string
}

// newPhoneHintFixture lists the fixture phone and one decoy under one WABA.
// Meta serves phone details only for the fixture phone, so selecting the decoy
// would fail validation; tests also assert the decoy is never read.
func newPhoneHintFixture(t *testing.T, smb bool) *phoneHintFixture {
	t.Helper()
	phoneID, wabaID := contractGraphIDs()
	decoyID, _ := contractGraphIDs()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	if smb {
		meta.phoneIsOnBizApp = true
		meta.phonePlatformType = "SMB_CLOUD_API"
	}
	meta.extraListedPhones = []whatsapp.WABAPhoneNumber{{
		ID:                 decoyID,
		DisplayPhoneNumber: phoneHintDecoyDisplay,
		VerifiedName:       "Synthetic Other Number",
	}}
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	return &phoneHintFixture{
		app:     app,
		meta:    meta,
		org:     org,
		user:    contractWriter(t, app, org.ID),
		phoneID: phoneID,
		wabaID:  wabaID,
		decoyID: decoyID,
	}
}

func (f *phoneHintFixture) exchange(t *testing.T, body map[string]any) *fastglue.Request {
	t.Helper()
	return f.exchangeFor(t, f.org.ID, f.user.ID, body)
}

func (f *phoneHintFixture) exchangeFor(t *testing.T, orgID, userID uuid.UUID, body map[string]any) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetHeader(req, "X-Organization-ID", orgID.String())
	require.NoError(t, f.app.ExchangeToken(req))
	return req
}

// codeAndWABA is Meta's documented Coexistence completion: the WABA ID only.
func (f *phoneHintFixture) codeAndWABA(code, mode, hint string) map[string]any {
	body := map[string]any{
		"code":        code,
		"signup_mode": mode,
		"waba_id":     f.wabaID,
	}
	if hint != "" {
		body["phone_number_hint"] = hint
	}
	return body
}

func (f *phoneHintFixture) rowsFor(t *testing.T, phoneID string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.app.DB.Unscoped().Model(&models.WhatsAppAccount{}).
		Where("BTRIM(phone_id) = ?", phoneID).
		Count(&count).Error)
	return count
}

func (f *phoneHintFixture) assertNoProviderMutation(t *testing.T) {
	t.Helper()
	for _, phoneID := range []string{f.phoneID, f.decoyID} {
		assert.Zero(t, f.meta.hit("/v21.0/"+phoneID+"/register"))
		assert.Zero(t, f.meta.hit("/v21.0/"+phoneID+"/smb_app_data"))
	}
	assert.Zero(t, f.meta.methodHits(http.MethodPost, "/v21.0/"+f.wabaID+"/subscribed_apps"))
}

// assertPrivateNumbers proves a response never echoes the typed number or
// another number listed under the WABA.
func assertPrivateNumbers(t *testing.T, req *fastglue.Request) {
	t.Helper()
	body := string(testutil.GetResponseBody(req))
	assert.NotContains(t, body, phoneHintSelectedDigits)
	assert.NotContains(t, body, "12-345")
	assert.NotContains(t, body, phoneHintDecoyDigits)
	assert.NotContains(t, body, phoneHintDecoyDisplay)
}

func TestEmbeddedSignupCoexistencePhoneHintSelectsListedPhoneInMultiPhoneWABA(t *testing.T) {
	f := newPhoneHintFixture(t, true)
	// The live case: another number in the same WABA is already connected in a
	// different workspace. Choosing by the operator's number must leave it alone.
	otherOrg := testutil.CreateTestOrganization(t, f.app.DB)
	otherToken, err := appcrypto.Encrypt("synthetic-other-workspace-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	other := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: otherOrg.ID,
		Name:           "Other workspace number",
		PhoneID:        f.decoyID,
		BusinessID:     f.wabaID,
		AccessToken:    otherToken,
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, f.app.DB.Create(other).Error)

	req := f.exchange(t, f.codeAndWABA("synthetic-phone-hint-code", embeddedSignupModeCoexistence, "+60 12-345 6789"))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	var stored models.WhatsAppAccount
	require.NoError(t, f.app.DB.Where("organization_id = ? AND phone_id = ?", f.org.ID, f.phoneID).First(&stored).Error)
	assert.Equal(t, f.wabaID, stored.BusinessID)
	assert.True(t, stored.IsSMB)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/v21.0/"+f.phoneID), "the chosen phone is validated once")
	assert.Equal(t, 2, f.meta.hit("/v21.0/"+f.phoneID+"/smb_app_data"))
	assert.Zero(t, f.meta.hit("/v21.0/"+f.decoyID), "the other listed number is never read")
	assert.Zero(t, f.meta.hit("/v21.0/"+f.decoyID+"/smb_app_data"))
	assert.Zero(t, f.meta.hit("/v21.0/"+f.phoneID+"/register"))

	var otherCount int64
	require.NoError(t, f.app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ?", f.org.ID).
		Count(&otherCount).Error)
	assert.EqualValues(t, 1, otherCount, "only the chosen number joins the workspace")
	var unchanged models.WhatsAppAccount
	require.NoError(t, f.app.DB.Where("id = ?", other.ID).First(&unchanged).Error)
	assert.Equal(t, otherOrg.ID, unchanged.OrganizationID)
	assert.Equal(t, other.AccessToken, unchanged.AccessToken)
	assert.Equal(t, "active", unchanged.Status)
	assert.False(t, unchanged.IsSMB)
}

func TestEmbeddedSignupCoexistencePhoneHintFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configure   func(*phoneHintFixture)
		hint        string
		wantMessage string
	}{
		{
			name:        "number not listed in a multi-phone WABA",
			hint:        "+44 7700 900123",
			wantMessage: "the number ending in 0123 is not listed in the selected WhatsApp Business Account",
		},
		{
			name: "number not listed in a single-phone WABA",
			configure: func(f *phoneHintFixture) {
				f.meta.extraListedPhones = nil
			},
			hint:        "+44 7700 900123",
			wantMessage: "the number ending in 0123 is not listed in the selected WhatsApp Business Account",
		},
		{
			name: "single-phone WABA lists only a different number",
			configure: func(f *phoneHintFixture) {
				// Meta's list can lag a fresh onboarding and show only another number.
				f.meta.extraListedPhones = nil
				f.meta.listedDisplay = phoneHintDecoyDisplay
			},
			hint:        "+60 12-345 6789",
			wantMessage: "the number ending in 6789 is not listed",
		},
		{
			name: "several listed phones match the number",
			configure: func(f *phoneHintFixture) {
				f.meta.extraListedPhones = append(f.meta.extraListedPhones, whatsapp.WABAPhoneNumber{
					ID:                 f.decoyID + "1",
					DisplayPhoneNumber: "60 12 345 6789",
					VerifiedName:       "Synthetic Duplicate Number",
				})
			},
			hint:        "+60123456789",
			wantMessage: "more than one phone in the selected WhatsApp Business Account matches the number ending in 6789",
		},
		{
			name:        "several listed phones and no number",
			wantMessage: "enter the WhatsApp Business app number you are connecting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhoneHintFixture(t, true)
			if tc.configure != nil {
				tc.configure(f)
			}
			req := f.exchange(t, f.codeAndWABA("synthetic-phone-hint-refusal", embeddedSignupModeCoexistence, tc.hint))
			testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, tc.wantMessage)
			assertPrivateNumbers(t, req)
			assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/v21.0/"+f.wabaID+"/phone_numbers"))
			assert.Zero(t, f.meta.hit("/v21.0/"+f.phoneID), "no listed phone is read before one is chosen")
			assert.Zero(t, f.meta.hit("/v21.0/"+f.decoyID))
			f.assertNoProviderMutation(t)
			assert.Zero(t, f.rowsFor(t, f.phoneID))
			assert.Zero(t, f.rowsFor(t, f.decoyID))
		})
	}
}

func TestEmbeddedSignupCoexistenceSinglePhoneWABA(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint string
	}{
		{name: "without a number keeps the listed phone"},
		{name: "with the matching number in another format", hint: "(60) 12-345-6789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhoneHintFixture(t, true)
			f.meta.extraListedPhones = nil
			req := f.exchange(t, f.codeAndWABA("synthetic-single-phone-code", embeddedSignupModeCoexistence, tc.hint))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
			var stored models.WhatsAppAccount
			require.NoError(t, f.app.DB.Where("organization_id = ? AND phone_id = ?", f.org.ID, f.phoneID).First(&stored).Error)
			assert.True(t, stored.IsSMB)
			assert.Equal(t, "active", stored.Status)
			assert.Equal(t, 2, f.meta.hit("/v21.0/"+f.phoneID+"/smb_app_data"))
		})
	}
}

func TestEmbeddedSignupPhoneHintIsIgnoredWhenMetaSuppliesThePhone(t *testing.T) {
	f := newPhoneHintFixture(t, true)
	body := f.codeAndWABA("synthetic-supplied-phone-code", embeddedSignupModeCoexistence, phoneHintDecoyDisplay)
	body["phone_id"] = f.phoneID
	req := f.exchange(t, body)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	assert.EqualValues(t, 1, f.rowsFor(t, f.phoneID), "Meta's phone ID wins over the typed number")
	assert.Zero(t, f.rowsFor(t, f.decoyID))
	assert.Zero(t, f.meta.hit("/v21.0/"+f.decoyID))
	// The only phone listing is the membership check for Meta's phone ID.
	assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/v21.0/"+f.wabaID+"/phone_numbers"))
}

func TestEmbeddedSignupClassicIgnoresPhoneHint(t *testing.T) {
	t.Run("multi-phone discovery still requires Meta's phone ID", func(t *testing.T) {
		f := newPhoneHintFixture(t, false)
		req := f.exchange(t, f.codeAndWABA("synthetic-classic-hint-code", embeddedSignupModeClassic, "+60 12-345 6789"))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "reconnect and select exactly one phone number")
		f.assertNoProviderMutation(t)
		assert.Zero(t, f.rowsFor(t, f.phoneID))
	})
	t.Run("a malformed number does not block a supplied phone", func(t *testing.T) {
		f := newPhoneHintFixture(t, false)
		body := f.codeAndWABA("synthetic-classic-malformed-hint", embeddedSignupModeClassic, "not a phone number")
		body["phone_id"] = f.phoneID
		req := f.exchange(t, body)
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
		var stored models.WhatsAppAccount
		require.NoError(t, f.app.DB.Where("organization_id = ? AND phone_id = ?", f.org.ID, f.phoneID).First(&stored).Error)
		assert.False(t, stored.IsSMB)
		assert.Equal(t, 1, f.meta.hit("/v21.0/"+f.phoneID+"/register"))
	})
}

func TestEmbeddedSignupCoexistenceRejectsMalformedPhoneHintBeforeMeta(t *testing.T) {
	f := newPhoneHintFixture(t, true)
	for _, hint := range []string{"012-345 6789", "+60 12 345 6789 ext 1", "60+123456789", "+1234567890123456"} {
		body := f.codeAndWABA("synthetic-malformed-hint-code", embeddedSignupModeCoexistence, hint)
		body["phone_id"] = f.phoneID
		req := f.exchange(t, body)
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "phone_number_hint must be the full international phone number")
		assert.NotContains(t, string(testutil.GetResponseBody(req)), hint)
	}
	assert.Zero(t, f.meta.totalHits(), "a malformed number fails before the one-time code is spent")
}

func TestEmbeddedSignupPhoneHintChoiceKeepsEveryExistingCheck(t *testing.T) {
	t.Run("the token must grant the WABA", func(t *testing.T) {
		f := newPhoneHintFixture(t, true)
		f.meta.granularTargetIDs = []string{testutil.NewTestGraphObjectID()}
		req := f.exchange(t, f.codeAndWABA("synthetic-hint-ungranted", embeddedSignupModeCoexistence, "+60123456789"))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "not granted")
		assert.Zero(t, f.meta.hit("/v21.0/"+f.wabaID+"/phone_numbers"), "the hint never widens the token's grant")
		f.assertNoProviderMutation(t)
	})
	t.Run("the token must grant the WABA for messaging", func(t *testing.T) {
		f := newPhoneHintFixture(t, true)
		f.meta.messagingTargetIDs = []string{testutil.NewTestGraphObjectID()}
		req := f.exchange(t, f.codeAndWABA("synthetic-hint-ungranted-messaging", embeddedSignupModeCoexistence, "+60123456789"))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "not granted for messaging")
		assert.Zero(t, f.meta.hit("/v21.0/"+f.wabaID+"/phone_numbers"))
		f.assertNoProviderMutation(t)
	})
	t.Run("the chosen phone must be a Business app number", func(t *testing.T) {
		f := newPhoneHintFixture(t, false)
		req := f.exchange(t, f.codeAndWABA("synthetic-hint-mode-mismatch", embeddedSignupModeCoexistence, "+60123456789"))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "does not match")
		f.assertNoProviderMutation(t)
		assert.Zero(t, f.rowsFor(t, f.phoneID))
	})
	t.Run("a number connected in another workspace stays there", func(t *testing.T) {
		f := newPhoneHintFixture(t, true)
		ownerOrg := testutil.CreateTestOrganization(t, f.app.DB)
		ownerToken, err := appcrypto.Encrypt("synthetic-owner-token", integrationTestEncryptionKey)
		require.NoError(t, err)
		owner := &models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: ownerOrg.ID,
			Name:           "Owner workspace number",
			PhoneID:        f.phoneID,
			BusinessID:     f.wabaID,
			AccessToken:    ownerToken,
			APIVersion:     "v21.0",
			Status:         "active",
		}
		require.NoError(t, f.app.DB.Create(owner).Error)

		req := f.exchange(t, f.codeAndWABA("synthetic-hint-global-conflict", embeddedSignupModeCoexistence, "+60123456789"))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already connected")
		assert.NotContains(t, string(testutil.GetResponseBody(req)), ownerOrg.ID.String())
		f.assertNoProviderMutation(t)
		var targetCount int64
		require.NoError(t, f.app.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ?", f.org.ID).
			Count(&targetCount).Error)
		assert.Zero(t, targetCount)
		var unchanged models.WhatsAppAccount
		require.NoError(t, f.app.DB.Where("id = ?", owner.ID).First(&unchanged).Error)
		assert.Equal(t, ownerOrg.ID, unchanged.OrganizationID)
		assert.Equal(t, owner.AccessToken, unchanged.AccessToken)
		assert.False(t, unchanged.IsSMB)
	})
	t.Run("a workspace without accounts:write is refused before Meta", func(t *testing.T) {
		f := newPhoneHintFixture(t, true)
		readerRole := testutil.CreateTestRoleWithKeys(t, f.app.DB, f.org.ID, "phone-hint-reader", []string{"accounts:read"})
		reader := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(&readerRole.ID))
		req := f.exchangeFor(t, f.org.ID, reader.ID, f.codeAndWABA("synthetic-hint-reader", embeddedSignupModeCoexistence, "+60123456789"))
		assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
		assert.Zero(t, f.meta.totalHits())
	})
}

// A #219 conversion of a live classic account whose stored API version differs
// from the configured one keeps that version when the account's number is
// chosen by the operator's hint among several listed phones.
func TestEmbeddedSignupPhoneHintKeepsLiveAccountAPIVersion(t *testing.T) {
	f := newCoexistenceVersionFixture(t)
	decoyID, _ := contractGraphIDs()
	f.meta.extraListedPhones = []whatsapp.WABAPhoneNumber{{
		ID:                 decoyID,
		DisplayPhoneNumber: phoneHintDecoyDisplay,
		VerifiedName:       "Synthetic Other Number",
	}}
	account := f.liveClassicAccount(t, coexistenceStoredAPIVersion)
	require.NoError(t, f.app.DB.Create(&account).Error)

	body := f.signupBody("synthetic-hint-kept-version", embeddedSignupModeCoexistence)
	delete(body, "phone_id")
	body["phone_number_hint"] = "+60 12-345 6789"
	req := f.exchange(t, f.org.ID, f.user.ID, body)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))

	stored := f.reload(t, account.ID)
	assert.Equal(t, coexistenceStoredAPIVersion, stored.APIVersion)
	assert.True(t, stored.IsSMB)
	assert.Equal(t, "active", stored.Status)
	assert.EqualValues(t, 1, f.phoneRows(t))
	// Discovery runs on the configured version; the chosen account's provider
	// calls then use its kept version.
	assert.Equal(t, 1, f.meta.methodHits(http.MethodGet, "/"+coexistenceConfiguredAPIVersion+"/"+f.wabaID+"/phone_numbers"))
	assert.Positive(t, f.meta.methodHits(http.MethodGet, "/"+coexistenceStoredAPIVersion+"/"+f.phoneID))
	assert.Equal(t, 2, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+f.phoneID+"/smb_app_data"))
	assert.Zero(t, f.meta.hit("/"+coexistenceStoredAPIVersion+"/"+decoyID))
	assert.Zero(t, f.meta.hit("/"+coexistenceConfiguredAPIVersion+"/"+decoyID))
	f.assertNoRegistrationOrSubscriptionWrite(t)
}

func (f *coexistenceVersionFixture) assertNoRegistrationOrSubscriptionWrite(t *testing.T) {
	t.Helper()
	for _, version := range []string{coexistenceStoredAPIVersion, coexistenceConfiguredAPIVersion} {
		assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+version+"/"+f.phoneID+"/register"), version)
		assert.Zero(t, f.meta.methodHits(http.MethodPost, "/"+version+"/"+f.wabaID+"/subscribed_apps"), version)
	}
}
