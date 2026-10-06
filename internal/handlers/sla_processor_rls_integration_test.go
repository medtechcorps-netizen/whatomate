package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// slaNoticeDelivery is what the synthetic provider saw for one SLA notice,
// plus the transfer state another connection could see at that moment.
type slaNoticeDelivery struct {
	To              string
	Body            string
	Status          models.TransferStatus
	EscalationLevel int
}

// slaNoticeProvider records every text send and reads the matching
// transfer's committed state through observer while the request is in flight.
func slaNoticeProvider(t *testing.T, app *App, observer *gorm.DB) func() []slaNoticeDelivery {
	t.Helper()
	return slaNoticeProviderWithHook(t, app, observer, nil)
}

// slaNoticeProviderWithHook calls beforeResponse with each recipient before
// recording the send, so a test can hold a send in flight.
func slaNoticeProviderWithHook(
	t *testing.T,
	app *App,
	observer *gorm.DB,
	beforeResponse func(to string),
) func() []slaNoticeDelivery {
	t.Helper()
	var (
		mu         sync.Mutex
		deliveries []slaNoticeDelivery
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			To   string `json:"to"`
			Text struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if beforeResponse != nil {
			beforeResponse(payload.To)
		}
		delivery := slaNoticeDelivery{To: payload.To, Body: payload.Text.Body}
		var transfer models.AgentTransfer
		if err := observer.Where("phone_number = ?", payload.To).First(&transfer).Error; err == nil {
			delivery.Status = transfer.Status
			delivery.EscalationLevel = transfer.SLA.EscalationLevel
		}
		mu.Lock()
		deliveries = append(deliveries, delivery)
		call := len(deliveries)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.sla-notice-%d-%s", call, uuid.NewString())}},
		})
	}))
	t.Cleanup(provider.Close)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, provider.URL)
	return func() []slaNoticeDelivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]slaNoticeDelivery(nil), deliveries...)
	}
}

// TestSLAProcessorSendsCustomerNoticesAfterTenantCommitWithRuntimeRoleRLS
// runs one real SLA tick through the restricted runtime role. Under RLS the
// whole organization tick is one tenant transaction, and expiring a transfer
// takes the organization policy fence (FOR UPDATE) inside it. A customer
// notice sent while that transaction is still open inserts its Message from
// another connection; the platform-compliance write guard then waits for the
// organization row FOR SHARE behind the tick's own fence until the 30 s send
// timeout, and the notice is lost. Every notice must be delivered after the
// tick commits, so the provider sees the transfer's new state as committed.
// It is not parallel: applying and removing RLS policies changes shared schema.
func TestSLAProcessorSendsCustomerNoticesAfterTenantCommitWithRuntimeRoleRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_sla_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
		runtimePassword,
	)).Error)
	t.Cleanup(func() {
		// Cleanup is best-effort so a test failure is not hidden by teardown.
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	organization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	account := testutil.CreateTestWhatsAppAccount(t, adminDB, organization.ID)
	const (
		autoCloseNotice = "Synthetic SLA auto-close notice"
		warningNotice   = "Synthetic SLA warning notice"
	)
	require.NoError(t, adminDB.Create(&models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		SLA: models.SLAConfig{
			Enabled:           true,
			AutoCloseHours:    2,
			AutoCloseMessage:  autoCloseNotice,
			EscalationMinutes: 30,
			WarningMessage:    warningNotice,
		},
	}).Error)

	past := time.Now().UTC().Add(-time.Hour)
	createTransfer := func(sla models.SLATracking) *models.AgentTransfer {
		contact := testutil.CreateTestContactWith(
			t,
			adminDB,
			organization.ID,
			testutil.WithContactAccount(account.Name),
			testutil.WithPhoneNumber("60"+testutil.NewTestGraphObjectID()[:10]),
		)
		transfer := &models.AgentTransfer{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  organization.ID,
			ContactID:       contact.ID,
			WhatsAppAccount: account.Name,
			PhoneNumber:     contact.PhoneNumber,
			Status:          models.TransferStatusActive,
			Source:          models.TransferSourceManual,
			TransferredAt:   past.Add(-time.Hour),
			SLA:             sla,
		}
		require.NoError(t, adminDB.Create(transfer).Error)
		return transfer
	}
	// Two expiries: the first takes the fence, so the second notice is the
	// one that used to queue behind it. The escalation runs after both.
	firstExpired := createTransfer(models.SLATracking{ExpiresAt: &past})
	secondExpired := createTransfer(models.SLATracking{ExpiresAt: &past})
	escalated := createTransfer(models.SLATracking{EscalationAt: &past})

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	app := newProcessorTestApp(t)
	app.DB = runtimeDB
	app.Config = &config.Config{
		App: config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
	}
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole
	app.WSHub = websocket.NewHub(app.Log)
	go app.WSHub.Run()
	deliveries := slaNoticeProvider(t, app, adminDB)

	// Record every runtime-role statement that waits on a lock during the
	// tick. A notice queued behind the tick's own fence shows up here.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	var lockWaits []string
	go func() {
		defer close(watchDone)
		seen := map[string]bool{}
		for {
			var waiting []string
			_ = adminDB.Raw(`
				SELECT query FROM pg_catalog.pg_stat_activity
				 WHERE usename = ? AND wait_event_type = 'Lock'`,
				runtimeRole,
			).Scan(&waiting).Error
			for _, query := range waiting {
				if !seen[query] {
					seen[query] = true
					lockWaits = append(lockWaits, query)
				}
			}
			select {
			case <-stopWatch:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	started := time.Now()
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		NewSLAProcessor(app, time.Minute).processStaleTransfers()
	}()
	select {
	case <-tickDone:
	case <-time.After(3 * time.Minute):
		t.Fatal("SLA tick did not finish")
	}
	elapsed := time.Since(started)
	close(stopWatch)
	<-watchDone
	t.Logf("SLA tick finished in %s", elapsed)

	// Every send used to be bounded by its 30 s timeout; a tick that waited on
	// its own fence takes at least that long.
	assert.Less(t, elapsed, 20*time.Second,
		"the SLA tick waited on its own organization policy fence; lock waits: %q", lockWaits)
	assert.Empty(t, lockWaits, "no SLA notice may wait on the tick's own transaction")

	byPhone := map[string]slaNoticeDelivery{}
	for _, delivery := range deliveries() {
		_, duplicate := byPhone[delivery.To]
		assert.False(t, duplicate, "each SLA notice is delivered once: %s", delivery.To)
		byPhone[delivery.To] = delivery
	}
	require.Len(t, byPhone, 3, "every SLA notice must reach the provider: %+v", deliveries())
	for _, expired := range []*models.AgentTransfer{firstExpired, secondExpired} {
		delivery := byPhone[expired.PhoneNumber]
		assert.Equal(t, autoCloseNotice, delivery.Body)
		assert.Equal(t, models.TransferStatusExpired, delivery.Status,
			"the auto-close notice is sent only after the expiry commits")
	}
	warning := byPhone[escalated.PhoneNumber]
	assert.Equal(t, warningNotice, warning.Body)
	assert.Equal(t, models.TransferStatusActive, warning.Status)
	assert.Equal(t, 1, warning.EscalationLevel, "the warning is sent only after the escalation commits")

	for _, expected := range []struct {
		transfer *models.AgentTransfer
		status   models.TransferStatus
		level    int
		body     string
	}{
		{firstExpired, models.TransferStatusExpired, 0, autoCloseNotice},
		{secondExpired, models.TransferStatusExpired, 0, autoCloseNotice},
		{escalated, models.TransferStatusActive, 1, warningNotice},
	} {
		var stored models.AgentTransfer
		require.NoError(t, adminDB.First(&stored, "id = ?", expected.transfer.ID).Error)
		assert.Equal(t, expected.status, stored.Status)
		assert.Equal(t, expected.level, stored.SLA.EscalationLevel)
		var messages []models.Message
		require.NoError(t, adminDB.Where(
			"organization_id = ? AND contact_id = ? AND direction = ?",
			organization.ID,
			expected.transfer.ContactID,
			models.DirectionOutgoing,
		).Find(&messages).Error)
		require.Len(t, messages, 1)
		assert.Equal(t, expected.body, messages[0].Content)
		assert.Equal(t, models.MessageStatusSent, messages[0].Status)
	}
}
