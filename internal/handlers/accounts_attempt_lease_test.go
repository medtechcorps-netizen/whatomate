package handlers

import (
	"encoding/json"
	"errors"
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
)

func newWhatsAppLeaseTestAccount(
	t *testing.T,
	app *App,
	orgID, userID uuid.UUID,
	phoneID, wabaID, status string,
) *models.WhatsAppAccount {
	t.Helper()

	accessToken, err := appcrypto.Encrypt("lease-test-access-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	appSecret, err := appcrypto.Encrypt("lease-test-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	pin, err := appcrypto.Encrypt("654321", integrationTestEncryptionKey)
	require.NoError(t, err)
	tokenExpiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)

	account := &models.WhatsAppAccount{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         orgID,
		Name:                   "Lease test " + uuid.NewString(),
		AppID:                  "lease-test-meta-app",
		PhoneID:                phoneID,
		BusinessID:             wabaID,
		AccessToken:            accessToken,
		AccessTokenExpiresAt:   &tokenExpiry,
		AppSecret:              appSecret,
		WebhookVerifyToken:     "lease-test-webhook-token",
		APIVersion:             "v21.0",
		Status:                 status,
		Pin:                    pin,
		CreatedByID:            &userID,
		UpdatedByID:            &userID,
		IsDefaultIncoming:      false,
		IsDefaultOutgoing:      false,
		AutoReadReceipt:        false,
		BusinessCallingEnabled: false,
	}
	require.NoError(t, app.DB.Create(account).Error)
	return account
}

func startEmbeddedSignupLeaseForTest(
	t *testing.T,
	app *App,
	orgID uuid.UUID,
	account *models.WhatsAppAccount,
) uuid.UUID {
	t.Helper()

	// Match production preflight semantics: compare against values reloaded from
	// the database, including the database's timestamp precision.
	preflight := loadWhatsAppLeaseTestAccount(t, app, orgID, account.ID)
	candidate := preflight
	candidate.Status = "pending_registration"
	attemptID := uuid.New()
	existing, _, err := app.claimEmbeddedSignupAccount(orgID, &candidate, attemptID, &preflight)
	require.NoError(t, err)
	require.True(t, existing)
	return attemptID
}

func loadWhatsAppLeaseTestAccount(
	t *testing.T,
	app *App,
	orgID, accountID uuid.UUID,
) models.WhatsAppAccount {
	t.Helper()

	var account models.WhatsAppAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", accountID, orgID).First(&account).Error)
	return account
}

func assertWhatsAppLeaseUnchanged(
	t *testing.T,
	before, after models.WhatsAppAccount,
) {
	t.Helper()

	require.NotNil(t, before.ConnectionAttemptID)
	require.NotNil(t, after.ConnectionAttemptID)
	assert.Equal(t, *before.ConnectionAttemptID, *after.ConnectionAttemptID)
	require.NotNil(t, before.ConnectionAttemptStartedAt)
	require.NotNil(t, after.ConnectionAttemptStartedAt)
	assert.True(t, before.ConnectionAttemptStartedAt.Equal(*after.ConnectionAttemptStartedAt))
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.AccessToken, after.AccessToken)
	assert.Equal(t, before.AccessTokenExpiresAt, after.AccessTokenExpiresAt)
	assert.Equal(t, before.AppSecret, after.AppSecret)
	assert.Equal(t, before.WebhookVerifyToken, after.WebhookVerifyToken)
	assert.Equal(t, before.Pin, after.Pin)
}

func TestLiveEmbeddedSignupAttemptBlocksManualMetaMutations(t *testing.T) {
	t.Run("RegisterPhoneNumber", func(t *testing.T) {
		phoneID := "lease-register-phone-" + uuid.NewString()
		wabaID := "lease-register-waba-" + uuid.NewString()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		app := newWhatsAppContractApp(t, meta)
		org := testutil.CreateTestOrganization(t, app.DB)
		user := contractWriter(t, app, org.ID)
		account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, wabaID, "active")
		startEmbeddedSignupLeaseForTest(t, app, org.ID, account)
		before := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)

		req := testutil.NewJSONRequest(t, map[string]any{"pin": "123456"})
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", account.ID.String())
		require.NoError(t, app.RegisterPhoneNumber(req))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already in progress")

		assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"), "Meta registration must not run behind a live lease")
		after := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		assertWhatsAppLeaseUnchanged(t, before, after)
	})

	t.Run("SubscribeApp", func(t *testing.T) {
		phoneID := "lease-subscribe-phone-" + uuid.NewString()
		wabaID := "lease-subscribe-waba-" + uuid.NewString()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		app := newWhatsAppContractApp(t, meta)
		org := testutil.CreateTestOrganization(t, app.DB)
		user := contractWriter(t, app, org.ID)
		account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, wabaID, "active")
		startEmbeddedSignupLeaseForTest(t, app, org.ID, account)
		before := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)

		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", account.ID.String())
		require.NoError(t, app.SubscribeApp(req))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already in progress")

		assert.Zero(t, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"), "Meta subscription must not run behind a live lease")
		after := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		assertWhatsAppLeaseUnchanged(t, before, after)
	})

	t.Run("contract-changing UpdateAccount", func(t *testing.T) {
		phoneID := "lease-update-phone-" + uuid.NewString()
		oldWABAID := "lease-update-old-waba-" + uuid.NewString()
		newWABAID := "lease-update-new-waba-" + uuid.NewString()
		meta := newWhatsAppContractMeta(t, phoneID, newWABAID)
		app := newWhatsAppContractApp(t, meta)
		org := testutil.CreateTestOrganization(t, app.DB)
		user := contractWriter(t, app, org.ID)
		account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, oldWABAID, "active")
		startEmbeddedSignupLeaseForTest(t, app, org.ID, account)
		before := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)

		req := testutil.NewJSONRequest(t, map[string]any{"business_id": newWABAID})
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", account.ID.String())
		require.NoError(t, app.UpdateAccount(req))
		testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already in progress")

		assert.Equal(t, 1, meta.hit("/v21.0/"+newWABAID+"/phone_numbers"), "read-only tuple validation should complete before claiming the mutation lease")
		assert.Zero(t, meta.hit("/v21.0/"+newWABAID+"/subscribed_apps"), "Meta subscription must not run behind a live lease")
		after := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		assertWhatsAppLeaseUnchanged(t, before, after)
		assert.Equal(t, oldWABAID, after.BusinessID)
	})
}

func TestNonContractUpdatePreservesLiveAttemptStateAndCredentials(t *testing.T) {
	phoneID := "lease-safe-update-phone-" + uuid.NewString()
	wabaID := "lease-safe-update-waba-" + uuid.NewString()
	meta := newWhatsAppContractMeta(t, phoneID, wabaID)
	app := newWhatsAppContractApp(t, meta)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, wabaID, "active")
	startEmbeddedSignupLeaseForTest(t, app, org.ID, account)
	before := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
	updatedName := "Safe lease update " + uuid.NewString()

	req := testutil.NewJSONRequest(t, map[string]any{
		"name":                     updatedName,
		"auto_read_receipt":        true,
		"business_calling_enabled": true,
	})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	after := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
	assertWhatsAppLeaseUnchanged(t, before, after)
	assert.Equal(t, updatedName, after.Name)
	assert.True(t, after.AutoReadReceipt)
	assert.True(t, after.BusinessCallingEnabled)
	assert.Equal(t, before.PhoneID, after.PhoneID)
	assert.Equal(t, before.BusinessID, after.BusinessID)
	assert.Equal(t, before.APIVersion, after.APIVersion)
	assert.Zero(t, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))
	assert.Zero(t, meta.hit("/v21.0/"+phoneID+"/register"))
}

func TestContractUpdateRejectsStaleReadBeforeMetaMutation(t *testing.T) {
	phoneID := "lease-stale-phone-" + uuid.NewString()
	oldWABAID := "lease-stale-old-waba-" + uuid.NewString()
	newWABAID := "lease-stale-new-waba-" + uuid.NewString()
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var lookupOnce sync.Once
	var releaseOnce sync.Once
	var subscriptionCalls atomic.Int32
	release := func() { releaseOnce.Do(func() { close(releaseLookup) }) }
	defer release()

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+newWABAID+"/subscribed_apps"):
			subscriptionCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case strings.HasSuffix(r.URL.Path, "/"+newWABAID+"/phone_numbers"):
			lookupOnce.Do(func() { close(lookupStarted) })
			<-releaseLookup
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{
					"id":                   phoneID,
					"display_phone_number": "+60123456789",
					"verified_name":        "Stale-read fixture",
					"quality_rating":       "GREEN",
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/"+phoneID):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                       phoneID,
				"display_phone_number":     "+60123456789",
				"verified_name":            "Stale-read fixture",
				"code_verification_status": "VERIFIED",
				"account_mode":             "LIVE",
				"quality_rating":           "GREEN",
				"platform_type":            "CLOUD_API",
			})
		case strings.HasSuffix(r.URL.Path, "/"+newWABAID):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": newWABAID, "name": "Stale-read WABA"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer metaServer.Close()

	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.WhatsApp.BaseURL = metaServer.URL
	app.Config.WhatsApp.APIVersion = "v21.0"
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := contractWriter(t, app, org.ID)
	account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, oldWABAID, "active")
	initial := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)

	req := testutil.NewJSONRequest(t, map[string]any{"business_id": newWABAID})
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "id", account.ID.String())
	result := make(chan error, 1)
	go func() { result <- app.UpdateAccount(req) }()

	waitForWhatsAppLeaseSignal(t, lookupStarted, "UpdateAccount did not pause in contract validation")
	rotatedToken, err := appcrypto.Encrypt("lease-test-rotated-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	rotatedPIN, err := appcrypto.Encrypt("987654", integrationTestEncryptionKey)
	require.NoError(t, err)
	concurrentUpdatedAt := initial.UpdatedAt.Add(time.Minute)
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, org.ID).
		Updates(map[string]any{
			"access_token": rotatedToken,
			"pin":          rotatedPIN,
			"updated_at":   concurrentUpdatedAt,
		}).Error)

	release()
	require.NoError(t, waitForWhatsAppLeaseResult(t, result, "UpdateAccount did not finish after validation was released"))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusConflict, "already in progress")
	assert.Zero(t, subscriptionCalls.Load(), "a stale account snapshot must be rejected before Meta mutation")

	stored := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
	assert.Equal(t, rotatedToken, stored.AccessToken)
	assert.Equal(t, rotatedPIN, stored.Pin)
	assert.True(t, stored.UpdatedAt.Equal(concurrentUpdatedAt))
	assert.Equal(t, oldWABAID, stored.BusinessID)
	assert.Equal(t, "active", stored.Status)
	assert.Nil(t, stored.ConnectionAttemptID)
}

func TestManualCreateAndSubscribeAttemptsBlockEmbeddedSignupClaim(t *testing.T) {
	t.Run("CreateAccount subscription", func(t *testing.T) {
		phoneID := "lease-create-visible-phone-" + uuid.NewString()
		wabaID := "lease-create-visible-waba-" + uuid.NewString()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		app := newWhatsAppContractApp(t, meta)
		org := testutil.CreateTestOrganization(t, app.DB)
		user := contractWriter(t, app, org.ID)

		subscribeStarted := make(chan struct{})
		releaseSubscribe := make(chan struct{})
		var startedOnce sync.Once
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseSubscribe) }) }
		defer release()
		meta.onSubscribe = func() {
			startedOnce.Do(func() { close(subscribeStarted) })
			<-releaseSubscribe
		}

		req := testutil.NewJSONRequest(t, map[string]any{
			"name":         "Visible create lease " + uuid.NewString(),
			"phone_id":     phoneID,
			"business_id":  wabaID,
			"access_token": "visible-create-token",
		})
		testutil.SetAuthContext(req, org.ID, user.ID)
		result := make(chan error, 1)
		go func() { result <- app.CreateAccount(req) }()

		waitForWhatsAppLeaseSignal(t, subscribeStarted, "CreateAccount did not reach Meta subscription")
		var pending models.WhatsAppAccount
		require.NoError(t, app.DB.Where("organization_id = ? AND phone_id = ?", org.ID, phoneID).First(&pending).Error)
		require.Equal(t, "pending_subscription", pending.Status)
		require.NotNil(t, pending.ConnectionAttemptID)
		manualAttemptID := *pending.ConnectionAttemptID

		candidate := pending
		candidate.Status = "pending_registration"
		_, _, claimErr := app.claimEmbeddedSignupAccount(org.ID, &candidate, uuid.New(), &pending)
		require.ErrorIs(t, claimErr, errEmbeddedSignupAlreadyInProgress)

		stillPending := loadWhatsAppLeaseTestAccount(t, app, org.ID, pending.ID)
		require.NotNil(t, stillPending.ConnectionAttemptID)
		assert.Equal(t, manualAttemptID, *stillPending.ConnectionAttemptID)
		assert.Equal(t, "pending_subscription", stillPending.Status)
		assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

		release()
		require.NoError(t, waitForWhatsAppLeaseResult(t, result, "CreateAccount did not finish after Meta was released"))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		finished := loadWhatsAppLeaseTestAccount(t, app, org.ID, pending.ID)
		assert.Equal(t, "active", finished.Status)
		assert.Nil(t, finished.ConnectionAttemptID)
	})

	t.Run("SubscribeApp", func(t *testing.T) {
		phoneID := "lease-subscribe-visible-phone-" + uuid.NewString()
		wabaID := "lease-subscribe-visible-waba-" + uuid.NewString()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		app := newWhatsAppContractApp(t, meta)
		org := testutil.CreateTestOrganization(t, app.DB)
		user := contractWriter(t, app, org.ID)
		account := newWhatsAppLeaseTestAccount(t, app, org.ID, user.ID, phoneID, wabaID, "active")

		subscribeStarted := make(chan struct{})
		releaseSubscribe := make(chan struct{})
		var startedOnce sync.Once
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseSubscribe) }) }
		defer release()
		meta.onSubscribe = func() {
			startedOnce.Do(func() { close(subscribeStarted) })
			<-releaseSubscribe
		}

		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		testutil.SetPathParam(req, "id", account.ID.String())
		result := make(chan error, 1)
		go func() { result <- app.SubscribeApp(req) }()

		waitForWhatsAppLeaseSignal(t, subscribeStarted, "SubscribeApp did not reach Meta subscription")
		pending := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		require.Equal(t, "pending_subscription", pending.Status)
		require.NotNil(t, pending.ConnectionAttemptID)
		manualAttemptID := *pending.ConnectionAttemptID

		candidate := pending
		candidate.Status = "pending_registration"
		_, _, claimErr := app.claimEmbeddedSignupAccount(org.ID, &candidate, uuid.New(), &pending)
		require.True(t, errors.Is(claimErr, errEmbeddedSignupAlreadyInProgress))

		stillPending := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		require.NotNil(t, stillPending.ConnectionAttemptID)
		assert.Equal(t, manualAttemptID, *stillPending.ConnectionAttemptID)
		assert.Equal(t, "pending_subscription", stillPending.Status)
		assert.Equal(t, 1, meta.hit("/v21.0/"+wabaID+"/subscribed_apps"))

		release()
		require.NoError(t, waitForWhatsAppLeaseResult(t, result, "SubscribeApp did not finish after Meta was released"))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		finished := loadWhatsAppLeaseTestAccount(t, app, org.ID, account.ID)
		assert.Equal(t, "active", finished.Status)
		assert.Nil(t, finished.ConnectionAttemptID)
	})
}

func waitForWhatsAppLeaseSignal(t *testing.T, signal <-chan struct{}, failureMessage string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal(failureMessage)
	}
}

func waitForWhatsAppLeaseResult(t *testing.T, result <-chan error, failureMessage string) error {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatal(failureMessage)
		return nil
	}
}
