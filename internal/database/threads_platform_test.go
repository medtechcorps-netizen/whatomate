package database_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPrepareProviderIntegrationManagementModeBackfillsLegacyRowsAsBYO(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	organization := testutil.CreateTestOrganization(t, db)
	require.NoError(t, db.Exec(`
		ALTER TABLE provider_integrations
		DROP CONSTRAINT IF EXISTS chk_provider_integrations_management_mode
	`).Error)
	require.NoError(t, db.Exec(`
		ALTER TABLE provider_integrations
		DROP COLUMN IF EXISTS management_mode
	`).Error)
	t.Cleanup(func() {
		_ = database.PrepareProviderIntegrationManagementMode(db)
		_ = db.AutoMigrate(&models.ProviderIntegration{})
		_ = database.CreateIndexes(db)
		testutil.TruncateTables(db)
	})

	legacyID := uuid.New()
	require.NoError(t, db.Exec(`
		INSERT INTO provider_integrations
			(id, created_at, updated_at, organization_id, provider, enabled, config, credential_data)
		VALUES (?, ?, ?, ?, 'threads', false, '{}'::jsonb, '{}'::jsonb)
	`, legacyID, time.Now().UTC(), time.Now().UTC(), organization.ID).Error)

	require.NoError(t, database.PrepareProviderIntegrationManagementMode(db))
	var mode string
	require.NoError(t, db.Raw(
		"SELECT management_mode FROM provider_integrations WHERE id = ?",
		legacyID,
	).Scan(&mode).Error)
	assert.Equal(t, models.ThreadsManagementModeWorkspaceBYO, mode)

	var column struct {
		Nullable      string
		ColumnDefault string
	}
	require.NoError(t, db.Raw(`
		SELECT is_nullable AS nullable, COALESCE(column_default, '') AS column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'provider_integrations'
		  AND column_name = 'management_mode'
	`).Scan(&column).Error)
	assert.Equal(t, "NO", column.Nullable)
	assert.Contains(t, column.ColumnDefault, models.ThreadsManagementModeWorkspaceBYO)

	secondOrganization := testutil.CreateTestOrganization(t, db)
	newLegacyBinaryID := uuid.New()
	require.NoError(t, db.Exec(`
		INSERT INTO provider_integrations
			(id, created_at, updated_at, organization_id, provider, enabled, config, credential_data)
		VALUES (?, ?, ?, ?, 'threads', false, '{}'::jsonb, '{}'::jsonb)
	`, newLegacyBinaryID, time.Now().UTC(), time.Now().UTC(), secondOrganization.ID).Error)
	require.NoError(t, db.Raw(
		"SELECT management_mode FROM provider_integrations WHERE id = ?",
		newLegacyBinaryID,
	).Scan(&mode).Error)
	assert.Equal(t, models.ThreadsManagementModeWorkspaceBYO, mode)
}

func TestManagedThreadsPersistenceConstraintKeepsPlatformSecretsOutOfTenantRows(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testutil.TruncateTables(db)
	appKey := "primary"

	byoOrganization := testutil.CreateTestOrganization(t, db)
	byo := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: byoOrganization.ID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModeWorkspaceBYO,
		Config:         models.JSONB{"app_id": "123456789012345"},
		CredentialData: models.JSONB{"app_secret": "synthetic-encrypted-byo-secret"},
	}
	require.NoError(t, db.Create(&byo).Error, "the existing BYO shape must remain valid")

	secretOrganization := testutil.CreateTestOrganization(t, db)
	managedWithSecret := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: secretOrganization.ID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &appKey,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{"app_secret": "must-not-be-tenant-owned"},
	}
	err := db.Create(&managedWithSecret).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_provider_integrations_management_mode")

	var managedCount int64
	require.NoError(t, db.Model(&models.ProviderIntegration{}).
		Where("id = ?", managedWithSecret.ID).
		Count(&managedCount).Error)
	assert.Zero(t, managedCount)

	nullKeyOrganization := testutil.CreateTestOrganization(t, db)
	managedWithoutKey := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: nullKeyOrganization.ID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	err = db.Create(&managedWithoutKey).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_provider_integrations_management_mode")

	validOrganization := testutil.CreateTestOrganization(t, db)
	validManaged := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: validOrganization.ID,
		Provider:       "threads",
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &appKey,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, db.Create(&validManaged).Error)
}

func TestThreadsPlatformBindingsAreTenantRLSProtected(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	require.NoError(t, database.RemoveTenantRLS(adminDB))
	runtimeRole := "rereply_threads_platform_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
	)).Error)
	t.Cleanup(func() {
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	organizationA := testutil.CreateTestOrganization(t, adminDB)
	organizationB := testutil.CreateTestOrganization(t, adminDB)
	for index, organizationID := range []uuid.UUID{organizationA.ID, organizationB.ID} {
		integration := models.ProviderIntegration{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: organizationID,
			Provider:       "threads",
			ManagementMode: models.ThreadsManagementModePlatformManaged,
			PlatformAppKey: stringPointer("primary"),
			Config:         models.JSONB{},
			CredentialData: models.JSONB{},
		}
		require.NoError(t, adminDB.Create(&integration).Error)
		binding := models.ThreadsPlatformBinding{
			BaseModel:               models.BaseModel{ID: uuid.New()},
			OrganizationID:          organizationID,
			IntegrationID:           integration.ID,
			PlatformAppKey:          "primary",
			PlatformAppID:           "123456789012345",
			OAuthSubjectID:          fmt.Sprintf("20000000000000%d", index),
			AuthorityAssetID:        fmt.Sprintf("30000000000000%d", index),
			ConfigurationGeneration: 7,
			AuthorizationGeneration: 1,
			Status:                  models.ThreadsPlatformBindingStatusPending,
			ClaimedAt:               time.Now().UTC(),
		}
		require.NoError(t, adminDB.Create(&binding).Error)
	}

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	require.NoError(t, adminDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE " + runtimeRole).Error; err != nil {
			return err
		}
		var unscoped int64
		if err := tx.Model(&models.ThreadsPlatformBinding{}).Count(&unscoped).Error; err != nil {
			return err
		}
		require.Zero(t, unscoped)
		return database.WithTenant(tx, organizationA.ID, func(tenantDB *gorm.DB) error {
			var bindings []models.ThreadsPlatformBinding
			if err := tenantDB.Find(&bindings).Error; err != nil {
				return err
			}
			require.Len(t, bindings, 1)
			require.Equal(t, organizationA.ID, bindings[0].OrganizationID)
			return nil
		})
	}))
}

func stringPointer(value string) *string { return &value }
