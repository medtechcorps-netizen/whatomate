package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type coexistenceSyncStub struct {
	mu             sync.Mutex
	phoneID        string
	contactsStatus int
	historyStatus  int
	contactsHits   int
	historyHits    int
	bodies         []map[string]any
	authorizations []string
}

func (stub *coexistenceSyncStub) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v21.0/"+stub.phoneID+"/smb_app_data" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	stub.mu.Lock()
	stub.bodies = append(stub.bodies, body)
	stub.authorizations = append(stub.authorizations, r.Header.Get("Authorization"))
	syncType, _ := body["sync_type"].(string)
	var status int
	requestID := "history-request"
	if syncType == string(whatsapp.CoexistenceSyncContacts) {
		stub.contactsHits++
		status = stub.contactsStatus
		requestID = "contacts-request"
	} else {
		stub.historyHits++
		status = stub.historyStatus
	}
	stub.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message":      "synthetic provider body must not be persisted",
				"code":         100,
				"is_transient": status >= http.StatusInternalServerError,
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"messaging_product": "whatsapp",
		"request_id":        requestID,
	})
}

func (stub *coexistenceSyncStub) authorizationHeaders() []string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]string(nil), stub.authorizations...)
}

func waitForCoexistenceLifecycleBlocker(t *testing.T, db *gorm.DB, blockerPID int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiters int64
		err := db.Raw(`
			SELECT COUNT(*)
			  FROM pg_catalog.pg_stat_activity AS activity
			 WHERE ? = ANY(pg_catalog.pg_blocking_pids(activity.pid))
		`, blockerPID).Scan(&waiters).Error
		return err == nil && waiters > 0
	}, 5*time.Second, 10*time.Millisecond, "provider boundary did not wait for the account lifecycle writer")
}

func newCoexistenceOnboardingFixture(t *testing.T, stub *coexistenceSyncStub) (*App, models.WhatsAppAccount, func()) {
	t.Helper()
	stub.phoneID = testutil.NewTestGraphObjectID()
	server := httptest.NewServer(http.HandlerFunc(stub.handler))
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	accountSuffix := uuid.NewString()[:8]
	account := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Coexistence Test " + accountSuffix,
		PhoneID:        stub.phoneID,
		BusinessID:     testutil.NewTestGraphObjectID(),
		AccessToken:    "coexistence-test-token",
		APIVersion:     "v21.0",
		Status:         "active",
		IsSMB:          true,
	}
	require.NoError(t, db.Create(&account).Error)
	app := &App{
		Config:   &config.Config{},
		DB:       db,
		Log:      testutil.NopLogger(),
		WhatsApp: whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL),
	}
	return app, account, server.Close
}

func TestBeginCoexistenceOnboardingRequestsBothOneTimeSyncs(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	state, err := app.beginCoexistenceOnboarding(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.HistorySyncStatus)
	assert.Equal(t, "contacts-request", state.ContactSyncRequestID)
	assert.Equal(t, "history-request", state.HistorySyncRequestID)
	assert.Equal(t, "15550000001", state.BusinessPhoneNumber)
	assert.Equal(t, models.CoexistenceOnboardingStatusConnected, state.OnboardingStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.SyncStatus)
	assert.Equal(t, 1, stub.contactsHits)
	assert.Equal(t, 1, stub.historyHits)
	for _, body := range stub.bodies {
		assert.Equal(t, "whatsapp", body["messaging_product"])
	}
}

func TestCoexistenceSyncDisconnectCommittedAtProviderBoundarySettlesWithoutMeta(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	initialized, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, initialized.OnboardedAt)

	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	committed := false
	defer func() {
		if !committed {
			_ = blocker.Rollback().Error
		}
	}()
	var blockerPID int
	require.NoError(t, blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error)
	require.NoError(t, blocker.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Update("status", "disconnected").Error)

	type syncResult struct {
		state     *models.WhatsAppCoexistenceState
		attempted bool
		err       error
	}
	result := make(chan syncResult, 1)
	go func() {
		state, attempted, runErr := app.runCoexistenceSyncRequests(
			context.Background(),
			account.OrganizationID,
			account.ID,
			account.ToWAAccount(),
		)
		result <- syncResult{state: state, attempted: attempted, err: runErr}
	}()

	waitForCoexistenceLifecycleBlocker(t, app.DB, blockerPID)
	require.NoError(t, blocker.Commit().Error)
	committed = true

	select {
	case outcome := <-result:
		require.NoError(t, outcome.err)
		require.True(t, outcome.attempted, "the prepared stage must be durably settled")
		require.NotNil(t, outcome.state)
		assert.Equal(t, models.CoexistenceSyncStatusFailed, outcome.state.ContactSyncStatus)
		assert.Equal(t, "account_inactive", outcome.state.ContactSyncErrorCode)
		assert.Equal(t, models.CoexistenceSyncStatusFailed, outcome.state.SyncStatus)
		assert.Equal(t, models.CoexistenceOnboardingStatusFailed, outcome.state.OnboardingStatus)
	case <-time.After(5 * time.Second):
		t.Fatal("coexistence sync did not settle after lifecycle commit")
	}
	assert.Zero(t, stub.contactsHits)
	assert.Zero(t, stub.historyHits)
}

func TestCoexistenceSyncCredentialRotationCommittedAtProviderBoundaryUsesLiveToken(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	initialized, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, initialized.OnboardedAt)
	rotatedToken := "coexistence-rotated-token-" + uuid.NewString()

	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	committed := false
	defer func() {
		if !committed {
			_ = blocker.Rollback().Error
		}
	}()
	var blockerPID int
	require.NoError(t, blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error)
	require.NoError(t, blocker.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, account.OrganizationID).
		Update("access_token", rotatedToken).Error)

	result := make(chan error, 1)
	go func() {
		state, attempted, runErr := app.runCoexistenceSyncRequests(
			context.Background(),
			account.OrganizationID,
			account.ID,
			account.ToWAAccount(),
		)
		if runErr == nil && (!attempted || state == nil ||
			state.ContactSyncStatus != models.CoexistenceSyncStatusRequested ||
			state.HistorySyncStatus != models.CoexistenceSyncStatusRequested) {
			runErr = errors.New("coexistence sync did not complete both provider stages")
		}
		result <- runErr
	}()

	waitForCoexistenceLifecycleBlocker(t, app.DB, blockerPID)
	require.NoError(t, blocker.Commit().Error)
	committed = true
	select {
	case runErr := <-result:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("coexistence sync did not finish after credential rotation")
	}
	assert.Equal(t, []string{
		"Bearer " + rotatedToken,
		"Bearer " + rotatedToken,
	}, stub.authorizationHeaders())
}

func TestCoexistenceSyncRollupSettlesAcceptedHistoryWithoutCallback(t *testing.T) {
	syncStatus, onboardingStatus, complete := coexistenceSyncRollup(
		models.CoexistenceSyncStatusRequested,
		models.CoexistenceSyncStatusRequested,
	)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, syncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusConnected, onboardingStatus)
	assert.False(t, complete)

	syncStatus, onboardingStatus, complete = coexistenceSyncRollup(
		models.CoexistenceSyncStatusRequested,
		models.CoexistenceSyncStatusCompleted,
	)
	assert.Equal(t, models.CoexistenceSyncStatusCompleted, syncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusReady, onboardingStatus)
	assert.True(t, complete)
}

func TestCoexistenceSyncRetriesOnlyDefiniteFailure(t *testing.T) {
	stub := &coexistenceSyncStub{contactsStatus: http.StatusBadRequest}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	state, err := app.beginCoexistenceOnboarding(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusFailed, state.ContactSyncStatus)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.HistorySyncStatus)
	assert.Equal(t, "100", state.ContactSyncErrorCode)
	assert.NotContains(t, state.ContactSyncErrorMessage, "synthetic provider body")

	stub.contactsStatus = http.StatusOK
	state, attempted, err := app.runCoexistenceSyncRequests(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
	)
	require.NoError(t, err)
	assert.True(t, attempted)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
	assert.Equal(t, 2, stub.contactsHits)
	assert.Equal(t, 1, stub.historyHits, "an accepted one-time history request must not be replayed")
}

func TestCoexistenceSyncDoesNotReplayAmbiguousRequest(t *testing.T) {
	stub := &coexistenceSyncStub{contactsStatus: http.StatusInternalServerError}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	state, err := app.beginCoexistenceOnboarding(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusRequesting, state.ContactSyncStatus)
	assert.NotContains(t, state.ContactSyncErrorMessage, "synthetic provider body")

	_, attempted, err := app.runCoexistenceSyncRequests(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
	)
	require.NoError(t, err)
	assert.False(t, attempted)
	assert.Equal(t, 1, stub.contactsHits, "an ambiguous one-time request must not be replayed")
	assert.Equal(t, 1, stub.historyHits, "an already accepted request must not be replayed")
}

func TestCoexistenceSyncExpiresBeforeProviderMutation(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	_, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC().Add(-models.CoexistenceSyncWindow-time.Minute),
		"15550000001",
	)
	require.NoError(t, err)

	_, attempted, err := app.runCoexistenceSyncRequests(
		context.Background(),
		account.OrganizationID,
		account.ID,
		account.ToWAAccount(),
	)
	require.ErrorIs(t, err, errCoexistenceSyncExpired)
	assert.False(t, attempted)
	assert.Zero(t, stub.contactsHits)
	assert.Zero(t, stub.historyHits)

	state, loadErr := app.loadCoexistenceState(account.OrganizationID, account.ID)
	require.NoError(t, loadErr)
	assert.Equal(t, models.CoexistenceOnboardingStatusExpired, state.OnboardingStatus)
	assert.Equal(t, models.CoexistenceSyncStatusExpired, state.ContactSyncStatus)
}

// This reads through a separate database handle after prepare has returned, so
// a rolled-back update cannot masquerade as a successfully persisted expiry.
func assertCoexistenceExpiryCommitted(
	t *testing.T,
	app *App,
	account models.WhatsAppAccount,
	readDB *gorm.DB,
	stage coexistenceSyncStage,
) {
	t.Helper()
	initialized, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC().Add(-models.CoexistenceSyncWindow-time.Minute),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, initialized.OnboardedAt)

	// The other stage may already have been accepted. Expiring this pending
	// stage must retain the acknowledgement and must never manufacture an attempt.
	acceptedColumn := "history_sync_status"
	if stage == coexistenceHistoryStage {
		acceptedColumn = "contact_sync_status"
	}
	require.NoError(t, app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
		return scoped.DB.Model(&models.WhatsAppCoexistenceState{}).
			Where("id = ?", initialized.ID).
			Update(acceptedColumn, models.CoexistenceSyncStatusRequested).Error
	}))

	snapshot, err := app.prepareCoexistenceRequest(
		account.OrganizationID,
		account.ID,
		stage,
		*initialized.OnboardedAt,
	)
	require.ErrorIs(t, err, errCoexistenceSyncExpired)
	require.Nil(t, snapshot)

	var persisted models.WhatsAppCoexistenceState
	require.NoError(t, readDB.Where("id = ?", initialized.ID).First(&persisted).Error)
	assert.Equal(t, models.CoexistenceOnboardingStatusExpired, persisted.OnboardingStatus)
	assert.Equal(t, models.CoexistenceSyncStatusExpired, persisted.SyncStatus)
	assert.Equal(t, initialized.Version+1, persisted.Version)
	assert.Zero(t, persisted.ContactSyncAttempts)
	assert.Zero(t, persisted.HistorySyncAttempts)
	if stage == coexistenceContactStage {
		assert.Equal(t, models.CoexistenceSyncStatusExpired, persisted.ContactSyncStatus)
		assert.Equal(t, models.CoexistenceSyncStatusRequested, persisted.HistorySyncStatus)
	} else {
		assert.Equal(t, models.CoexistenceSyncStatusRequested, persisted.ContactSyncStatus)
		assert.Equal(t, models.CoexistenceSyncStatusExpired, persisted.HistorySyncStatus)
	}
}

func TestPrepareCoexistenceRequestCommitsExpiryWithoutRLS(t *testing.T) {
	for _, stage := range []coexistenceSyncStage{coexistenceContactStage, coexistenceHistoryStage} {
		t.Run(string(stage), func(t *testing.T) {
			app, account, closeServer := newCoexistenceOnboardingFixture(t, &coexistenceSyncStub{})
			defer closeServer()
			assertCoexistenceExpiryCommitted(t, app, account, testutil.SetupTestDB(t), stage)
		})
	}
}

func TestCoexistenceSyncPreservesAcceptedStagesPastDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     models.CoexistenceSyncStatus
		onboarding models.CoexistenceOnboardingStatus
		aggregate  models.CoexistenceSyncStatus
	}{
		{"requested", models.CoexistenceSyncStatusRequested, models.CoexistenceOnboardingStatusConnected, models.CoexistenceSyncStatusRequested},
		{"completed", models.CoexistenceSyncStatusCompleted, models.CoexistenceOnboardingStatusReady, models.CoexistenceSyncStatusCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &coexistenceSyncStub{}
			app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
			defer closeServer()
			initialized, err := app.initializeCoexistenceStateCommitted(
				account.OrganizationID,
				account.ID,
				time.Now().UTC().Add(-models.CoexistenceSyncWindow-time.Minute),
				"15550000001",
			)
			require.NoError(t, err)
			require.NoError(t, app.DB.Model(initialized).Updates(map[string]any{
				"contact_sync_status": tc.status,
				"history_sync_status": tc.status,
				"onboarding_status":   tc.onboarding,
				"sync_status":         tc.aggregate,
			}).Error)

			for _, stage := range []coexistenceSyncStage{coexistenceContactStage, coexistenceHistoryStage} {
				snapshot, err := app.prepareCoexistenceRequest(account.OrganizationID, account.ID, stage, *initialized.OnboardedAt)
				require.ErrorIs(t, err, errCoexistenceStageUnavailable)
				require.Nil(t, snapshot)
			}
			persisted, err := app.loadCoexistenceState(account.OrganizationID, account.ID)
			require.NoError(t, err)
			assert.Equal(t, initialized.Version, persisted.Version, "terminal stages must not persist an expiry transition")
			assert.Equal(t, tc.onboarding, persisted.OnboardingStatus)
			assert.Equal(t, tc.aggregate, persisted.SyncStatus)

			state, attempted, err := app.runCoexistenceSyncRequests(context.Background(), account.OrganizationID, account.ID, account.ToWAAccount())
			require.NoError(t, err)
			assert.False(t, attempted)
			assert.Equal(t, tc.status, state.ContactSyncStatus)
			assert.Equal(t, tc.status, state.HistorySyncStatus)
			assert.Equal(t, tc.onboarding, state.OnboardingStatus)
			assert.Equal(t, tc.aggregate, state.SyncStatus)
			assert.Zero(t, stub.contactsHits)
			assert.Zero(t, stub.historyHits)
		})
	}
}

func TestCoexistenceSyncExpiresPendingHistoryAfterAcceptedContact(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()
	initialized, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC().Add(-models.CoexistenceSyncWindow-time.Minute),
		"15550000001",
	)
	require.NoError(t, err)
	require.NoError(t, app.DB.Model(initialized).Updates(map[string]any{
		"contact_sync_status":     models.CoexistenceSyncStatusRequested,
		"contact_sync_request_id": "accepted-contact-request",
	}).Error)

	state, attempted, err := app.runCoexistenceSyncRequests(context.Background(), account.OrganizationID, account.ID, account.ToWAAccount())
	require.ErrorIs(t, err, errCoexistenceSyncExpired)
	require.Nil(t, state)
	assert.False(t, attempted)
	persisted, err := app.loadCoexistenceState(account.OrganizationID, account.ID)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, persisted.ContactSyncStatus)
	assert.Equal(t, "accepted-contact-request", persisted.ContactSyncRequestID)
	assert.Equal(t, models.CoexistenceSyncStatusExpired, persisted.HistorySyncStatus)
	assert.Equal(t, models.CoexistenceOnboardingStatusExpired, persisted.OnboardingStatus)
	assert.Equal(t, models.CoexistenceSyncStatusExpired, persisted.SyncStatus)
	assert.Equal(t, initialized.Version+1, persisted.Version)
	assert.Zero(t, persisted.ContactSyncAttempts)
	assert.Zero(t, persisted.HistorySyncAttempts)
	assert.Zero(t, stub.contactsHits)
	assert.Zero(t, stub.historyHits)
}

func TestCoexistenceFinalizeSurvivesIndependentStageUpdate(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	initialized, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, initialized.OnboardedAt)
	contactSnapshot, err := app.prepareCoexistenceRequest(
		account.OrganizationID,
		account.ID,
		coexistenceContactStage,
		*initialized.OnboardedAt,
	)
	require.NoError(t, err)

	// A different sync stage legitimately advances the shared state version
	// while the contact Graph request is in flight.
	require.NoError(t, app.DB.Model(&models.WhatsAppCoexistenceState{}).Where(
		"organization_id = ? AND whats_app_account_id = ?",
		account.OrganizationID,
		account.ID,
	).Updates(map[string]any{
		"history_sync_status": models.CoexistenceSyncStatusRequested,
		"version":             gorm.Expr("version + 1"),
	}).Error)

	require.NoError(t, app.finalizeCoexistenceRequest(
		account.OrganizationID,
		contactSnapshot,
		&whatsapp.CoexistenceSyncResponse{
			MessagingProduct: "whatsapp",
			RequestID:        "contacts-after-history-update",
		},
		nil,
	))
	state, err := app.loadCoexistenceState(account.OrganizationID, account.ID)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
	assert.Equal(t, "contacts-after-history-update", state.ContactSyncRequestID)
}

func TestCoexistenceReonboardingFencesDelayedPriorResponse(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	oldState, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC(),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, oldState.OnboardedAt)
	oldSnapshot, err := app.prepareCoexistenceRequest(
		account.OrganizationID,
		account.ID,
		coexistenceContactStage,
		*oldState.OnboardedAt,
	)
	require.NoError(t, err)

	newState, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		time.Now().UTC().Add(time.Second),
		"15550000001",
	)
	require.NoError(t, err)
	require.NotNil(t, newState.OnboardedAt)
	newSnapshot, err := app.prepareCoexistenceRequest(
		account.OrganizationID,
		account.ID,
		coexistenceContactStage,
		*newState.OnboardedAt,
	)
	require.NoError(t, err)
	assert.Greater(t, newState.OnboardingCycle, oldState.OnboardingCycle)
	assert.NotEqual(t, *oldState.OnboardedAt, *newState.OnboardedAt)
	assert.Equal(t, 1, oldSnapshot.attempt)
	assert.Equal(t, 1, newSnapshot.attempt)

	require.ErrorIs(t, app.finalizeCoexistenceRequest(
		account.OrganizationID,
		oldSnapshot,
		&whatsapp.CoexistenceSyncResponse{MessagingProduct: "whatsapp", RequestID: "stale-request"},
		nil,
	), errCoexistenceStateSuperseded)
	_, err = app.prepareCoexistenceRequest(
		account.OrganizationID,
		account.ID,
		coexistenceHistoryStage,
		*oldState.OnboardedAt,
	)
	require.ErrorIs(t, err, errCoexistenceStateSuperseded)
	require.NoError(t, app.finalizeCoexistenceRequest(
		account.OrganizationID,
		newSnapshot,
		&whatsapp.CoexistenceSyncResponse{MessagingProduct: "whatsapp", RequestID: "fresh-request"},
		nil,
	))

	state, err := app.loadCoexistenceState(account.OrganizationID, account.ID)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusRequested, state.ContactSyncStatus)
	assert.Equal(t, "fresh-request", state.ContactSyncRequestID)
	assert.Equal(t, 1, state.ContactSyncAttempts)
}

func TestCoexistenceSignupStartsFreshCycleOnlyAfterOffboarding(t *testing.T) {
	onboardedAt := time.Now().UTC().Truncate(time.Second)
	offboardedAt := onboardedAt.Add(time.Minute)
	reconnectedAt := offboardedAt.Add(time.Minute)
	base := models.WhatsAppCoexistenceState{
		OnboardedAt:      &onboardedAt,
		LifecycleStatus:  models.CoexistenceLifecycleStatusConnected,
		OnboardingStatus: models.CoexistenceOnboardingStatusReady,
	}

	assert.True(t, coexistenceSignupStartsFreshCycle(false, &base),
		"a classic-to-SMB transition starts its first coexistence cycle")
	assert.False(t, coexistenceSignupStartsFreshCycle(true, &base),
		"an active SMB credential refresh preserves accepted one-time requests")

	partnerRemoved := base
	partnerRemoved.LifecycleStatus = models.CoexistenceLifecycleStatusDisconnected
	partnerRemoved.LastLifecycleEvent = "PARTNER_REMOVED"
	assert.False(t, coexistenceSignupStartsFreshCycle(true, &partnerRemoved),
		"partner removal is not the offboarding Meta requires before request replay")

	offboarded := base
	offboarded.LifecycleStatus = models.CoexistenceLifecycleStatusOffboarded
	offboarded.OnboardingStatus = models.CoexistenceOnboardingStatusOffboarded
	offboarded.LastLifecycleEvent = "ACCOUNT_OFFBOARDED"
	offboarded.OffboardedAt = &offboardedAt
	assert.True(t, coexistenceSignupStartsFreshCycle(true, &offboarded))

	reconnected := base
	reconnected.LastLifecycleEvent = "ACCOUNT_RECONNECTED"
	reconnected.OffboardedAt = &offboardedAt
	reconnected.ReconnectedAt = &reconnectedAt
	assert.True(t, coexistenceSignupStartsFreshCycle(true, &reconnected))

	staleOffboardedAt := onboardedAt.Add(-time.Minute)
	reconnected.OffboardedAt = &staleOffboardedAt
	assert.False(t, coexistenceSignupStartsFreshCycle(true, &reconnected),
		"offboarding from an older cycle cannot authorize another request pair")
}

func TestInitializeCoexistenceStatePreservesNewerReconnectWatermark(t *testing.T) {
	stub := &coexistenceSyncStub{}
	app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
	defer closeServer()

	onboardedAt := time.Now().UTC().Truncate(time.Second)
	reconnectedAt := onboardedAt.Add(time.Second)
	existing := models.WhatsAppCoexistenceState{
		ID:                   uuid.New(),
		OrganizationID:       account.OrganizationID,
		WhatsAppAccountID:    account.ID,
		OnboardingStatus:     models.CoexistenceOnboardingStatusFailed,
		SyncStatus:           models.CoexistenceSyncStatusFailed,
		ContactSyncStatus:    models.CoexistenceSyncStatusFailed,
		HistoryConsent:       models.CoexistenceHistoryConsentUnknown,
		HistorySyncStatus:    models.CoexistenceSyncStatusFailed,
		LifecycleStatus:      models.CoexistenceLifecycleStatusConnected,
		LastLifecycleEvent:   "ACCOUNT_RECONNECTED",
		LastLifecycleEventAt: &reconnectedAt,
		ReconnectedAt:        &reconnectedAt,
		LifecycleMetadata:    models.JSONB{"event": "ACCOUNT_RECONNECTED"},
		Version:              4,
	}
	require.NoError(t, app.DB.Create(&existing).Error)

	state, err := app.initializeCoexistenceStateCommitted(
		account.OrganizationID,
		account.ID,
		onboardedAt,
		"15550000001",
	)
	require.NoError(t, err)
	assert.Equal(t, models.CoexistenceSyncStatusPending, state.ContactSyncStatus)
	assert.Equal(t, "ACCOUNT_RECONNECTED", state.LastLifecycleEvent)
	require.NotNil(t, state.LastLifecycleEventAt)
	assert.Equal(t, reconnectedAt, state.LastLifecycleEventAt.UTC())
	require.NotNil(t, state.ReconnectedAt)
	assert.Equal(t, reconnectedAt, state.ReconnectedAt.UTC())
}

func TestInitializeCoexistenceStateRejectsNewerTerminalLifecycle(t *testing.T) {
	for _, event := range []string{"PARTNER_REMOVED", "ACCOUNT_OFFBOARDED"} {
		t.Run(event, func(t *testing.T) {
			stub := &coexistenceSyncStub{}
			app, account, closeServer := newCoexistenceOnboardingFixture(t, stub)
			defer closeServer()

			onboardedAt := time.Now().UTC().Truncate(time.Second)
			eventAt := onboardedAt.Add(time.Second)
			existing := models.WhatsAppCoexistenceState{
				ID:                   uuid.New(),
				OrganizationID:       account.OrganizationID,
				WhatsAppAccountID:    account.ID,
				OnboardingStatus:     models.CoexistenceOnboardingStatusOffboarded,
				SyncStatus:           models.CoexistenceSyncStatusFailed,
				ContactSyncStatus:    models.CoexistenceSyncStatusFailed,
				HistoryConsent:       models.CoexistenceHistoryConsentUnknown,
				HistorySyncStatus:    models.CoexistenceSyncStatusFailed,
				LifecycleStatus:      models.CoexistenceLifecycleStatusOffboarded,
				LastLifecycleEvent:   event,
				LastLifecycleEventAt: &eventAt,
				LifecycleMetadata:    models.JSONB{"event": event},
				Version:              4,
			}
			require.NoError(t, app.DB.Create(&existing).Error)

			_, err := app.initializeCoexistenceStateCommitted(
				account.OrganizationID,
				account.ID,
				onboardedAt,
				"15550000001",
			)
			require.ErrorIs(t, err, errCoexistenceStateSuperseded)

			state, loadErr := app.loadCoexistenceState(account.OrganizationID, account.ID)
			require.NoError(t, loadErr)
			assert.Equal(t, event, state.LastLifecycleEvent)
			assert.Equal(t, models.CoexistenceSyncStatusFailed, state.SyncStatus)
		})
	}
}
