package handlers

import (
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestExchangeTokenRuntimeRoleRLSDirectRoute exercises the direct Embedded
// Signup handler through the same restricted PostgreSQL role and transaction-
// local tenant setting used in production. It is intentionally not parallel:
// applying and removing RLS policies changes shared test-database schema state.
func TestExchangeTokenRuntimeRoleRLSDirectRoute(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)

	runtimeRole := "rereply_exchange_" + uuid.NewString()[:8]
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
	homeOrg := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	memberTargetOrg := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	nonmemberTargetOrg := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	deletedTargetOrg := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	superTargetOrg := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)

	homeRole := testutil.CreateTestRoleWithKeys(
		t,
		adminDB,
		homeOrg.ID,
		"exchange-rls-home",
		[]string{"accounts:write"},
	)
	member := testutil.CreateTestUser(t, adminDB, homeOrg.ID, testutil.WithRoleID(&homeRole.ID))
	memberTargetRole := testutil.CreateTestRoleWithKeys(
		t,
		adminDB,
		memberTargetOrg.ID,
		"exchange-rls-member-target",
		[]string{"accounts:read", "accounts:write"},
	)
	require.NoError(t, adminDB.Create(&models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         member.ID,
		OrganizationID: memberTargetOrg.ID,
		RoleID:         &memberTargetRole.ID,
	}).Error)
	deletedTargetRole := testutil.CreateTestRoleWithKeys(
		t,
		adminDB,
		deletedTargetOrg.ID,
		"exchange-rls-deleted-target",
		[]string{"accounts:write"},
	)
	require.NoError(t, adminDB.Create(&models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         member.ID,
		OrganizationID: deletedTargetOrg.ID,
		RoleID:         &deletedTargetRole.ID,
	}).Error)
	require.NoError(t, adminDB.Delete(deletedTargetOrg).Error)

	superAdmin := testutil.CreateTestUser(t, adminDB, homeOrg.ID, testutil.WithSuperAdmin())

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))

	invoke := func(
		t *testing.T,
		meta *whatsappContractMeta,
		userID, targetOrgID uuid.UUID,
		code string,
	) int {
		t.Helper()
		app := newWhatsAppContractApp(t, meta)
		app.DB = runtimeDB
		app.Config.Database.RLSEnabled = true
		app.Config.Database.RuntimeRole = runtimeRole

		req := testutil.NewJSONRequest(t, map[string]any{
			"code":     code,
			"phone_id": meta.phoneID,
			"waba_id":  meta.wabaID,
		})
		testutil.SetAuthContext(req, homeOrg.ID, userID)
		testutil.SetHeader(req, "X-Organization-ID", targetOrgID.String())
		require.NoError(t, app.ExchangeToken(req))
		return testutil.GetResponseStatusCode(req)
	}

	assertSelectedAccount := func(t *testing.T, phoneID string, selectedOrgID uuid.UUID) {
		t.Helper()
		var selectedCount, otherCount int64
		require.NoError(t, adminDB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND phone_id = ?", selectedOrgID, phoneID).
			Count(&selectedCount).Error)
		require.NoError(t, adminDB.Model(&models.WhatsAppAccount{}).
			Where("organization_id <> ? AND phone_id = ?", selectedOrgID, phoneID).
			Count(&otherCount).Error)
		assert.Equal(t, int64(1), selectedCount)
		assert.Zero(t, otherCount, "the route must never fall back to the JWT/default organization")

		// The handler's committed tenant phases must leave no session-level
		// organization behind on a pooled runtime connection.
		var unscopedCount int64
		require.NoError(t, runtimeDB.Model(&models.WhatsAppAccount{}).
			Where("phone_id = ?", phoneID).
			Count(&unscopedCount).Error)
		assert.Zero(t, unscopedCount, "runtime-role reads must fail closed without tenant context")

		var selectedVisible int64
		require.NoError(t, database.WithTenant(runtimeDB, selectedOrgID, func(tx *gorm.DB) error {
			return tx.Model(&models.WhatsAppAccount{}).
				Where("phone_id = ?", phoneID).
				Count(&selectedVisible).Error
		}))
		assert.Equal(t, int64(1), selectedVisible)

		var homeVisible int64
		require.NoError(t, database.WithTenant(runtimeDB, homeOrg.ID, func(tx *gorm.DB) error {
			return tx.Model(&models.WhatsAppAccount{}).
				Where("phone_id = ?", phoneID).
				Count(&homeVisible).Error
		}))
		assert.Zero(t, homeVisible)
	}

	t.Run("alternate organization membership selects and writes only that organization", func(t *testing.T) {
		phoneID, wabaID := contractGraphIDs()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		status := invoke(t, meta, member.ID, memberTargetOrg.ID, "synthetic-rls-member-code")
		require.Equal(t, fasthttp.StatusOK, status)
		assert.Greater(t, meta.totalHits(), 0)
		assertSelectedAccount(t, phoneID, memberTargetOrg.ID)
	})

	t.Run("superadmin selects and writes only the exact active organization", func(t *testing.T) {
		phoneID, wabaID := contractGraphIDs()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		status := invoke(t, meta, superAdmin.ID, superTargetOrg.ID, "synthetic-rls-super-code")
		require.Equal(t, fasthttp.StatusOK, status)
		assert.Greater(t, meta.totalHits(), 0)
		assertSelectedAccount(t, phoneID, superTargetOrg.ID)
	})

	t.Run("nonmember target is rejected before Meta", func(t *testing.T) {
		phoneID, wabaID := contractGraphIDs()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		status := invoke(t, meta, member.ID, nonmemberTargetOrg.ID, "must-not-reach-meta-nonmember")
		require.Equal(t, fasthttp.StatusForbidden, status)
		assert.Zero(t, meta.totalHits())
		var accountCount int64
		require.NoError(t, adminDB.Model(&models.WhatsAppAccount{}).
			Where("phone_id = ?", phoneID).
			Count(&accountCount).Error)
		assert.Zero(t, accountCount)
	})

	t.Run("deleted target is rejected before Meta despite stale membership", func(t *testing.T) {
		phoneID, wabaID := contractGraphIDs()
		meta := newWhatsAppContractMeta(t, phoneID, wabaID)
		status := invoke(t, meta, member.ID, deletedTargetOrg.ID, "must-not-reach-meta-deleted")
		require.Equal(t, fasthttp.StatusForbidden, status)
		assert.Zero(t, meta.totalHits())
		var accountCount int64
		require.NoError(t, adminDB.Model(&models.WhatsAppAccount{}).
			Where("phone_id = ?", phoneID).
			Count(&accountCount).Error)
		assert.Zero(t, accountCount)
	})
}

func openRuntimeRoleTestDB(t *testing.T, role, password string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme,
		"TEST_DATABASE_URL must be a PostgreSQL URL for the runtime-role integration test")
	parsed.User = url.UserPassword(role, password)

	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
