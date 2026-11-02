package handlers

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
)

// Serial: ApplyTenantRLS changes the shared disposable fixture schema. This
// uses a real restricted login, not SET ROLE or an administrator connection.
// It installs synthetic vocabulary only; no production rollout is performed.
func TestChatExportPostgresRuntimeRoleRLSReadOnlyNoLeak(t *testing.T) {
	testutil.TruncateTables(testutil.SetupTestDB(t))
	f := newChatExportFixture(t)
	admin := f.app.DB
	reseller := testutil.CreateTestReseller(t, admin)
	require.NoError(t, admin.Model(&models.Organization{}).Where("id = ?", f.org.ID).Update("reseller_id", reseller.ID).Error)
	otherOrg := testutil.CreateTestOrganizationForReseller(t, admin, reseller.ID)
	otherAccount := testutil.CreateTestWhatsAppAccount(t, admin, otherOrg.ID)
	foreign := testutil.CreateTestContactWith(t, admin, otherOrg.ID, testutil.WithContactAccount(otherAccount.Name))
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	first := f.message(t, f.contact, f.account.Name, at)
	second := f.message(t, f.contact, f.account.Name, at.Add(time.Microsecond))
	foreignMessage := f.message(t, foreign, otherAccount.Name, at)

	runtimeRole := "rereply_export_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, admin.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
		runtimeRole, runtimePassword,
	)).Error)
	t.Cleanup(func() {
		if database.RemoveTenantRLS(admin) != nil {
			t.Error("remove disposable export RLS policies failed")
		}
		if admin.Exec("DROP OWNED BY "+runtimeRole).Error != nil {
			t.Error("remove disposable export runtime grants failed")
		}
		if admin.Exec("DROP ROLE "+runtimeRole).Error != nil {
			t.Error("remove disposable export runtime role failed")
		}
		testutil.TruncateTables(admin)
	})
	require.NoError(t, database.ApplyTenantRLS(admin, runtimeRole))
	runtime := testutil.OpenTestDBAsRole(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtime, runtimeRole))
	pool, err := runtime.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	app := &App{
		DB: runtime.WithContext(ctx), Log: testutil.NopLogger(),
		Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: true, RuntimeRole: runtimeRole}},
	}
	var nativeRestricted bool
	require.NoError(t, runtime.WithContext(ctx).Raw(`SELECT current_user=session_user AND current_user=?
 AND NOT rolsuper AND NOT rolbypassrls AND NOT rolcreaterole AND NOT rolreplication
 FROM pg_catalog.pg_roles WHERE rolname=current_user`, runtimeRole).Scan(&nativeRestricted).Error)
	require.True(t, nativeRestricted)
	var originalPID int
	require.NoError(t, runtime.WithContext(ctx).Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&originalPID).Error)
	require.Positive(t, originalPID)
	assertRootUnscoped := func() {
		t.Helper()
		var count int64
		require.NoError(t, runtime.WithContext(ctx).Model(&models.Message{}).Count(&count).Error)
		require.Zero(t, count, "unscoped runtime must not see either tenant")
		var tenant string
		require.NoError(t, runtime.WithContext(ctx).Raw("SELECT COALESCE(pg_catalog.current_setting('app.current_organization_id',true),'')").Scan(&tenant).Error)
		require.Empty(t, tenant, "the single reused connection must not retain tenant scope")
		var currentPID int
		require.NoError(t, runtime.WithContext(ctx).Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&currentPID).Error)
		require.Equal(t, originalPID, currentPID, "scope cleanup must be observed on the original physical connection")
	}
	assertRootUnscoped()

	req := chatExportRequest{Scope: "all", Account: "whatsapp:" + hex.EncodeToString([]byte(f.account.Name))}
	var archive *chatExportArchive
	var initial chatExportAccess
	err = app.chatExportReadTransaction(ctx, f.org.ID, true, func(scoped *App) error {
		var readOnly, isolation string
		if err := scoped.DB.Raw("SELECT current_setting('transaction_read_only'),current_setting('transaction_isolation')").Row().Scan(&readOnly, &isolation); err != nil {
			return err
		}
		require.Equal(t, "on", readOnly)
		require.Equal(t, "repeatable read", isolation)
		// No organization predicate: the actual database policies must hide the
		// foreign row even before the export reader applies its own fences.
		var visible []uuid.UUID
		if err := scoped.DB.Model(&models.Message{}).Where("id IN ?", []uuid.UUID{first.ID, second.ID, foreignMessage.ID}).Pluck("id", &visible).Error; err != nil {
			return err
		}
		require.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, visible)
		var err error
		initial, err = scoped.chatExportAccess(f.org.ID, f.user.ID)
		if err != nil {
			return err
		}
		require.False(t, initial.AllContacts, "fixture exercises an assigned nonadmin exporter")
		preview, err := scoped.chatExportPreview(f.org.ID, f.user.ID, req, initial)
		if err != nil {
			return err
		}
		require.EqualValues(t, 2, preview.Messages)
		require.EqualValues(t, 1, preview.Conversations)
		archive, err = newChatExportArchive(time.Now().UTC(), req)
		if err != nil {
			return err
		}
		return scoped.buildChatExport(ctx, f.org.ID, f.user.ID, req, initial, archive)
	})
	if archive != nil {
		t.Cleanup(archive.cleanup)
	}
	require.NoError(t, err)
	require.NotNil(t, archive)
	require.NoError(t, archive.finish())
	require.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, archive.IDs)
	assertRootUnscoped()
	require.NoError(t, app.revalidateChatExport(ctx, f.org.ID, f.user.ID, req, initial, archive.IDs))
	assertRootUnscoped()

	err = app.chatExportReadTransaction(ctx, f.org.ID, false, func(scoped *App) error {
		return scoped.DB.Model(&models.Message{}).Where("id = ?", first.ID).Update("content", "forbidden fixture write").Error
	})
	var state interface{ SQLState() string }
	require.ErrorAs(t, err, &state)
	require.Equal(t, "25006", state.SQLState(), "PostgreSQL must reject the write in the export transaction")
	assertRootUnscoped()
	var stored models.Message
	require.NoError(t, admin.First(&stored, "id = ?", first.ID).Error)
	require.Equal(t, first.Content, stored.Content)

	// Revoke using the isolated fixture administrator after releasing the
	// snapshot. The native runtime must re-read grants before delivery.
	require.NoError(t, admin.Model(f.role).Association("Permissions").Delete(&f.exportPermission))
	require.ErrorIs(t, app.revalidateChatExport(ctx, f.org.ID, f.user.ID, req, initial, archive.IDs), errChatExportDenied)
	assertRootUnscoped()
	var read int64
	require.NoError(t, admin.Model(&models.Message{}).Where("id IN ? AND status = ?", archive.IDs, models.MessageStatusRead).Count(&read).Error)
	require.Zero(t, read, "export must not acknowledge messages")
}
