package testutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// CreateTestPlatformComplianceOrganization installs the normal migration-owned
// guard contract when needed, then uses the production atomic creator. It never
// disables a row trigger or reclassifies an ordinary organization.
func CreateTestPlatformComplianceOrganization(
	t *testing.T,
	db *gorm.DB,
	enableInstagram, enableThreads bool,
) (*models.Reseller, *models.Organization) {
	t.Helper()
	return CreateTestPlatformComplianceOrganizationWithID(
		t, db, uuid.New(), enableInstagram, enableThreads,
	)
}

// CreateTestPlatformComplianceOrganizationWithID is the exact-ID variant used
// by configuration readers whose test configuration contains a fixed UUID.
func CreateTestPlatformComplianceOrganizationWithID(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	enableInstagram, enableThreads bool,
) (*models.Reseller, *models.Organization) {
	t.Helper()
	require.NotNil(t, db)
	require.NotEqual(t, uuid.Nil, organizationID)

	var reseller models.Reseller
	err := db.Unscoped().Where("slug = ?", database.PlatformResellerSlug).
		First(&reseller).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		reseller = models.Reseller{
			BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Synthetic Platform Direct",
			Slug: database.PlatformResellerSlug, Status: models.ResellerStatusActive,
			Plan: models.ResellerPlanEnterprise, MaxOrganizations: 1000, Settings: models.JSONB{},
		}
		require.NoError(t, db.Create(&reseller).Error)
	} else {
		require.NoError(t, err)
		require.NoError(t, db.Unscoped().Model(&reseller).Updates(map[string]any{
			"deleted_at": nil,
			"status":     models.ResellerStatusActive,
		}).Error)
	}

	ensurePlatformComplianceTestContract(t, db)
	runID := "test-atomic-create-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL search_path = pg_catalog, public").Error; err != nil {
			return err
		}
		return database.CreatePlatformComplianceOrganization(
			tx, organizationID, runID, enableInstagram, enableThreads,
		)
	}))

	var organization models.Organization
	require.NoError(t, db.Unscoped().Where("id = ?", organizationID).
		First(&organization).Error)
	t.Cleanup(func() {
		assertPlatformComplianceOrganizationGuardEnabled(t, db)
	})
	return &reseller, &organization
}

func ensurePlatformComplianceTestContract(t *testing.T, db *gorm.DB) {
	t.Helper()
	// A previous RemoveTenantRLS deliberately preserves the additive creator
	// and write guards, but removes the exact FORCE-RLS migration visibility
	// policies. Function existence therefore cannot prove the contract. Always
	// run the normal installer with a fresh role before invoking the creator.
	runtimeRole := "rereply_compliance_fixture_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	require.NoError(t, db.Exec(
		"CREATE ROLE "+runtimeRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
	).Error)
	t.Cleanup(func() {
		TruncateTables(db)
		require.NoError(t, database.RemoveTenantRLS(db))
		require.NoError(t, db.Exec("DROP OWNED BY "+runtimeRole).Error)
		require.NoError(t, db.Exec("DROP ROLE IF EXISTS "+runtimeRole).Error)
	})
	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
}

func assertPlatformComplianceOrganizationGuardEnabled(t *testing.T, db *gorm.DB) {
	t.Helper()
	var enabled bool
	require.NoError(t, db.Raw(`
		SELECT pg_catalog.count(*) = 1
			AND pg_catalog.bool_and(trigger.tgenabled = 'O')
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public'
		  AND relation.relname = 'organizations'
		  AND trigger.tgname = 'rereply_platform_compliance_classification_guard'
		  AND NOT trigger.tgisinternal
	`).Scan(&enabled).Error)
	require.True(t, enabled, "organization compliance fixture guard must remain enabled")
}
