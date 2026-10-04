package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestWhatsAppOrphanStatusWithRuntimeRoleRLS runs the orphan-status branch
// through the restricted PostgreSQL runtime role and the transaction-local
// tenant setting used in production. Under RLS the absence proof still sees
// this tenant's soft-deleted linked row, and another tenant's row carrying the
// same WAMID neither blocks the acknowledgement nor is touched. It is not
// parallel: applying and removing RLS policies changes shared test schema.
func TestWhatsAppOrphanStatusWithRuntimeRoleRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_status_" + uuid.NewString()[:8]
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
	otherOrganization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	account := testutil.CreateTestWhatsAppAccount(t, adminDB, organization.ID)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, adminDB, otherOrganization.ID)
	require.False(t, account.IsSMB)
	otherContact := testutil.CreateTestContact(t, adminDB, otherOrganization.ID)

	orphanWAMID := "wamid.synthetic-rls-orphan-" + uuid.NewString()
	otherRow := models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: otherOrganization.ID,
		ContactID: otherContact.ID, WhatsAppAccount: otherAccount.Name, WhatsAppMessageID: orphanWAMID,
		Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
		Content: "synthetic other-tenant row", Status: models.MessageStatusSent, Metadata: models.JSONB{},
	}
	require.NoError(t, adminDB.Create(&otherRow).Error)
	foreignWAMID := "wamid.synthetic-rls-foreign-" + uuid.NewString()
	foreign := createLinkedForeignWAMIDRow(t, adminDB, *account, foreignWAMID)
	require.NoError(t, adminDB.Delete(&foreign).Error)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	app := &App{
		Config: &config.Config{
			App:      config.AppConfig{EncryptionKey: "test-encryption-key-32-bytes-long"},
			WhatsApp: config.WhatsAppConfig{AppSecret: webhookTestAppSecret},
		},
		DB:  runtimeDB,
		Log: testutil.NopLogger(),
	}
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole

	at := func(age time.Duration) string { return strconv.FormatInt(time.Now().Add(-age).Unix(), 10) }

	err := app.processStatusUpdate(account.PhoneID, WebhookStatus{ID: orphanWAMID, Status: "read", Timestamp: at(0)})
	require.ErrorIs(t, err, errWhatsAppStatusOwnerPending, "a fresh orphan is still retried")

	require.NoError(t, app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: orphanWAMID, Status: "read", Timestamp: at(time.Hour),
	}), "another tenant's row must not block a settled orphan")
	assert.Equal(t, http.StatusOK, signedWebhookStatusCode(t, app, orphanStatusBody(t, *account,
		orphanStatusAt(orphanWAMID, "read", time.Now().Add(-time.Hour)))))

	err = app.processStatusUpdate(account.PhoneID, WebhookStatus{
		ID: foreignWAMID, Status: "delivered", Timestamp: at(6 * time.Hour),
	})
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.NotErrorIs(t, err, errWhatsAppStatusOwnerPending,
		"a soft-deleted row carrying the WAMID is visible to the proof under RLS")

	var storedOther models.Message
	require.NoError(t, adminDB.First(&storedOther, otherRow.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedOther.Status)
	var storedForeign models.Message
	require.NoError(t, adminDB.Unscoped().First(&storedForeign, foreign.ID).Error)
	assert.Equal(t, models.MessageStatusSent, storedForeign.Status)
	var ownRows int64
	require.NoError(t, adminDB.Unscoped().Model(&models.Message{}).
		Where("organization_id = ?", organization.ID).Count(&ownRows).Error)
	assert.Equal(t, int64(1), ownRows, "only the pre-existing soft-deleted row belongs to this tenant")
}
