package database_test

import (
	"errors"
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

// cleanAll truncates every table so each test starts with a blank slate.
func cleanAll(t *testing.T, db *gorm.DB) {
	t.Helper()
	testutil.TruncateTables(db)
}

func TestGlobalWhatsAppPhoneOwnershipAllowsOnlyOneActiveWorkspace(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)
	owner := testutil.CreateTestOrganization(t, db)
	claimant := testutil.CreateTestOrganization(t, db)
	phoneID := "global-owner-" + uuid.NewString()

	first := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: owner.ID,
		Name:           "First owner " + uuid.NewString(),
		PhoneID:        phoneID,
		BusinessID:     "waba-" + uuid.NewString(),
		AccessToken:    "token",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, db.Create(&first).Error)

	second := models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: claimant.ID,
		Name:           "Second owner " + uuid.NewString(),
		PhoneID:        phoneID,
		BusinessID:     "waba-" + uuid.NewString(),
		AccessToken:    "token",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	err := db.Create(&second).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), database.GlobalWhatsAppPhoneIDIndex)

	require.NoError(t, db.Delete(&first).Error)
	require.NoError(t, db.Create(&second).Error, "a soft-deleted connection must release the phone for transfer")
	require.NoError(t, database.EnsureGlobalWhatsAppPhoneIDUniqueness(db), "the migration must be idempotent")

	var indexDefinition string
	require.NoError(t, db.Raw(`
		SELECT indexdef
		FROM pg_catalog.pg_indexes
		WHERE schemaname = 'public' AND indexname = ?
	`, database.GlobalWhatsAppPhoneIDIndex).Scan(&indexDefinition).Error)
	assert.Contains(t, indexDefinition, "UNIQUE INDEX")
	assert.Contains(t, indexDefinition, "(phone_id)")
	assert.Contains(t, indexDefinition, "WHERE (deleted_at IS NULL)")
}

func TestGlobalWhatsAppPhoneOwnershipMigrationRefusesLegacyDuplicates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)
	errRollback := errors.New("rollback duplicate migration fixture")

	err := db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Exec("DROP INDEX public."+database.GlobalWhatsAppPhoneIDIndex).Error)
		firstOrg := testutil.CreateTestOrganization(t, tx)
		secondOrg := testutil.CreateTestOrganization(t, tx)
		phoneID := "legacy-duplicate-" + uuid.NewString()
		for index, organizationID := range []uuid.UUID{firstOrg.ID, secondOrg.ID} {
			require.NoError(t, tx.Create(&models.WhatsAppAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organizationID,
				Name:           "Legacy duplicate owner " + uuid.NewString(),
				PhoneID:        phoneID,
				BusinessID:     "waba-" + uuid.NewString(),
				AccessToken:    "token",
				APIVersion:     "v21.0",
				Status:         "active",
			}).Error, "insert legacy duplicate %d", index)
		}

		migrationErr := database.EnsureGlobalWhatsAppPhoneIDUniqueness(tx)
		require.Error(t, migrationErr)
		assert.Contains(t, migrationErr.Error(), "resolve duplicate active phone IDs first")

		var installedIndex *string
		require.NoError(t, tx.Raw(
			"SELECT to_regclass(?)::text",
			"public."+database.GlobalWhatsAppPhoneIDIndex,
		).Scan(&installedIndex).Error)
		assert.Nil(t, installedIndex, "unsafe index must not be installed after the preflight fails")
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
}

func TestBackfillProviderIntegrationBindingsRestoresLegacyThreadsAppID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)
	organization := testutil.CreateTestOrganization(t, db)
	legacy := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       "threads",
		Enabled:        false,
		Config:         models.JSONB{"app_id": "1553429782494481"},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, db.Create(&legacy).Error)
	require.Nil(t, legacy.ThreadsAppID)

	require.NoError(t, database.BackfillProviderIntegrationBindings(db))
	require.NoError(t, db.First(&legacy, "id = ?", legacy.ID).Error)
	require.NotNil(t, legacy.ThreadsAppID)
	assert.Equal(t, "1553429782494481", *legacy.ThreadsAppID)
}

// --- SeedPermissionsAndRoles ---

func TestSeedPermissionsAndRoles_CreatesAllDefaultPermissions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	err := database.SeedPermissionsAndRoles(db)
	require.NoError(t, err)

	var count int64
	db.Model(&models.Permission{}).Count(&count)

	expected := len(models.DefaultPermissions())
	assert.Equal(t, int64(expected), count, "all default permissions should be created")
}

func TestSeedPermissionsAndRoles_Idempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	// Seed twice
	require.NoError(t, database.SeedPermissionsAndRoles(db))
	require.NoError(t, database.SeedPermissionsAndRoles(db))

	var count int64
	db.Model(&models.Permission{}).Count(&count)

	expected := len(models.DefaultPermissions())
	assert.Equal(t, int64(expected), count, "idempotent: count should remain the same after two seeds")
}

func TestSeedPermissionsAndRoles_PermissionsHaveResourceAndAction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))

	var perms []models.Permission
	db.Find(&perms)

	for _, p := range perms {
		assert.NotEmpty(t, p.Resource, "permission resource must not be empty")
		assert.NotEmpty(t, p.Action, "permission action must not be empty")
		assert.NotEqual(t, uuid.Nil, p.ID, "permission ID must be set")
	}
}

// --- SeedSystemRolesForOrg ---

func TestSeedSystemRolesForOrg_CreatesThreeSystemRoles(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	// Need permissions first
	require.NoError(t, database.SeedPermissionsAndRoles(db))

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Test Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)

	err := database.SeedSystemRolesForOrg(db, org.ID)
	require.NoError(t, err)

	var roles []models.CustomRole
	db.Where("organization_id = ? AND is_system = ?", org.ID, true).Find(&roles)
	assert.Len(t, roles, 3, "should create admin, manager, agent roles")

	names := make(map[string]bool)
	for _, r := range roles {
		names[r.Name] = true
	}
	assert.True(t, names["admin"], "admin role should exist")
	assert.True(t, names["manager"], "manager role should exist")
	assert.True(t, names["agent"], "agent role should exist")
}

func TestSeedSystemRolesForOrg_Idempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Idempotent Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)

	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var count int64
	db.Model(&models.CustomRole{}).Where("organization_id = ? AND is_system = ?", org.ID, true).Count(&count)
	assert.Equal(t, int64(3), count, "idempotent: still exactly 3 system roles")
}

func TestSeedSystemRolesForOrg_AgentIsDefault(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Default Role Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var agentRole models.CustomRole
	err := db.Where("organization_id = ? AND name = ? AND is_system = ?", org.ID, "agent", true).First(&agentRole).Error
	require.NoError(t, err)
	assert.True(t, agentRole.IsDefault, "agent role should be the default role")
}

func TestSeedSystemRolesForOrg_AdminRoleHasAllPermissions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))

	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Admin Perms Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var adminRole models.CustomRole
	err := db.Where("organization_id = ? AND name = ? AND is_system = ?", org.ID, "admin", true).First(&adminRole).Error
	require.NoError(t, err)

	// Load permissions through the association
	var perms []models.Permission
	err = db.Model(&adminRole).Association("Permissions").Find(&perms)
	require.NoError(t, err)

	totalPerms := len(models.DefaultPermissions())
	assert.Equal(t, totalPerms, len(perms), "admin role should have all permissions")
}

func TestSeedSystemRolesForOrg_ManagerKeepsOperationsButNotSensitiveSettings(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Manager Policy Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var managerRole models.CustomRole
	require.NoError(t, db.Where(
		"organization_id = ? AND name = ? AND is_system = ?",
		org.ID,
		"manager",
		true,
	).First(&managerRole).Error)

	assert.True(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceSettingsGeneral, models.ActionRead,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceSettingsGeneral, models.ActionWrite,
	))
	assert.True(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceAccounts, models.ActionWrite,
	))
	assert.True(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceContacts, models.ActionWrite,
	))
	assert.True(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceCannedResponses, models.ActionWrite,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceWebhooks, models.ActionRead,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceCustomActions, models.ActionRead,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourcePrivacySettings, models.ActionRead,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceSupport, models.ActionRead,
	))

	var callingPermissionCount int64
	require.NoError(t, db.Model(&models.Permission{}).Where(
		"resource = ? AND action IN ?",
		models.ResourceSettingsCalling,
		[]string{models.ActionRead, models.ActionWrite},
	).Count(&callingPermissionCount).Error)
	assert.Equal(t, int64(2), callingPermissionCount, "calling settings permissions must be seeded")
}

func TestApplyManagerSettingsPolicyMigration_RunsOnceAndPreservesCustomRoles(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Legacy Manager Policy Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var managerRole models.CustomRole
	require.NoError(t, db.Where(
		"organization_id = ? AND name = ? AND is_system = ?",
		org.ID,
		"manager",
		true,
	).First(&managerRole).Error)

	var legacyPermissions []models.Permission
	require.NoError(t, db.Where(
		"(resource = ? AND action = ?) OR (resource = ? AND action = ?) OR (resource = ? AND action = ?)",
		models.ResourceSettingsGeneral,
		models.ActionWrite,
		models.ResourceWebhooks,
		models.ActionRead,
		models.ResourcePrivacySettings,
		models.ActionWrite,
	).Find(&legacyPermissions).Error)
	require.Len(t, legacyPermissions, 3)
	require.NoError(t, db.Model(&managerRole).Association("Permissions").Append(legacyPermissions))

	customRole := testutil.CreateTestRoleExact(
		t,
		db,
		org.ID,
		"custom-manager-equivalent",
		false,
		false,
		legacyPermissions,
	)

	// Simulate an organization created before the version marker existed.
	require.NoError(t, db.Where("id = ?", org.ID).First(&org).Error)
	delete(org.Settings, database.ManagerSettingsPolicyVersionKey)
	require.NoError(t, db.Model(&org).Update("settings", org.Settings).Error)

	require.NoError(t, database.ApplyManagerSettingsPolicyMigration(db))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceSettingsGeneral, models.ActionWrite,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceWebhooks, models.ActionRead,
	))
	assert.False(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourcePrivacySettings, models.ActionWrite,
	))
	assert.True(t, roleHasPermission(
		t, db, customRole.ID, models.ResourceSettingsGeneral, models.ActionWrite,
	), "custom roles must never be changed by the system-manager migration")

	require.NoError(t, db.Where("id = ?", org.ID).First(&org).Error)
	assert.NotNil(t, org.Settings[database.ManagerSettingsPolicyVersionKey])

	// A later explicit super-admin grant must survive application restarts.
	var generalWrite models.Permission
	require.NoError(t, db.Where(
		"resource = ? AND action = ?",
		models.ResourceSettingsGeneral,
		models.ActionWrite,
	).First(&generalWrite).Error)
	require.NoError(t, db.Model(&managerRole).Association("Permissions").Append(&generalWrite))
	require.NoError(t, database.ApplyManagerSettingsPolicyMigration(db))
	assert.True(t, roleHasPermission(
		t, db, managerRole.ID, models.ResourceSettingsGeneral, models.ActionWrite,
	), "the versioned migration must not reapply after an explicit later grant")
}

func TestFixSystemRolePermissionsAddsOnlyNewExpectedPermissions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	require.NoError(t, database.SeedPermissionsAndRoles(db))
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Role Upgrade Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&org).Error)
	require.NoError(t, database.SeedSystemRolesForOrg(db, org.ID))

	var agentRole models.CustomRole
	require.NoError(t, db.Where(
		"organization_id = ? AND name = ? AND is_system = ?",
		org.ID,
		"agent",
		true,
	).First(&agentRole).Error)

	var newlyIntroduced models.Permission
	require.NoError(t, db.Where(
		"resource = ? AND action = ?",
		models.ResourceConversations,
		models.ActionRead,
	).First(&newlyIntroduced).Error)
	require.NoError(t, db.Model(&agentRole).
		Association("Permissions").
		Delete(&newlyIntroduced))
	newPermissionTime := agentRole.UpdatedAt.Add(time.Minute)
	require.NoError(t, db.Model(&models.Permission{}).
		Where("id = ?", newlyIntroduced.ID).
		UpdateColumn("created_at", newPermissionTime).Error)

	require.NoError(t, database.FixSystemRolePermissions(db))
	assert.True(t, roleHasPermission(
		t,
		db,
		agentRole.ID,
		models.ResourceConversations,
		models.ActionRead,
	), "permission introduced after the role's last update should be added")

	var intentionallyRemoved models.Permission
	require.NoError(t, db.Where(
		"resource = ? AND action = ?",
		models.ResourceCRMLeads,
		models.ActionRead,
	).First(&intentionallyRemoved).Error)
	require.NoError(t, db.Model(&agentRole).
		Association("Permissions").
		Delete(&intentionallyRemoved))
	require.NoError(t, db.Model(&models.CustomRole{}).
		Where("id = ?", agentRole.ID).
		UpdateColumn("updated_at", time.Now().UTC().Add(time.Hour)).Error)

	require.NoError(t, database.FixSystemRolePermissions(db))
	assert.False(t, roleHasPermission(
		t,
		db,
		agentRole.ID,
		models.ResourceCRMLeads,
		models.ActionRead,
	), "an older permission removed after a role update must stay removed")
}

func roleHasPermission(
	t *testing.T,
	db *gorm.DB,
	roleID uuid.UUID,
	resource, action string,
) bool {
	t.Helper()
	var count int64
	require.NoError(t, db.Table("role_permissions AS rp").
		Joins("JOIN permissions AS p ON p.id = rp.permission_id").
		Where(
			"rp.custom_role_id = ? AND p.resource = ? AND p.action = ?",
			roleID,
			resource,
			action,
		).
		Count(&count).Error)
	return count > 0
}

func TestCreateIndexesEnforcesProductTenantAndLedgerIntegrity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)
	require.NoError(t, database.CreateIndexes(db))

	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	contactA := testutil.CreateTestContact(t, db, orgA.ID)
	serviceA := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgA.ID,
		Name:            "Tenant A service",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		Version:         1,
	}
	require.NoError(t, db.Create(&serviceA).Error)
	resourceB := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		Name:           "Tenant B practitioner",
		Kind:           models.BookingResourceKindPractitioner,
		Timezone:       "Asia/Kuala_Lumpur",
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, db.Create(&resourceB).Error)

	startsAt := time.Now().UTC().Add(time.Hour)
	crossTenantEvent := models.BookingEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		ServiceID:      serviceA.ID,
		ResourceID:     resourceB.ID,
		StartsAt:       startsAt,
		EndsAt:         startsAt.Add(30 * time.Minute),
		Capacity:       1,
		Status:         models.BookingEventStatusScheduled,
		Version:        1,
	}
	err := db.Create(&crossTenantEvent).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fk_booking_events_resource_tenant")

	inconsistentInvoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		ContactID:      contactA.ID,
		InvoiceNumber:  "INV-INCONSISTENT",
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  1000,
		TotalMinor:     1000,
		PaidMinor:      0,
		DueMinor:       500,
		Version:        1,
	}
	err = db.Create(&inconsistentInvoice).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_commerce_invoices_equation")

	channelB := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgB.ID,
		Channel:           models.ChannelWebChat,
		Provider:          "native",
		Name:              "Tenant B web chat",
		ExternalAccountID: "tenant-b-widget",
		Status:            models.ChannelAccountStatusActive,
		Capabilities:      models.JSONB{},
		Config:            models.JSONB{},
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&channelB).Error)

	crossTenantConversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         orgA.ID,
		ChannelAccountID:       channelB.ID,
		ContactID:              contactA.ID,
		Channel:                models.ChannelWebChat,
		ExternalConversationID: "cross-tenant-thread",
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               time.Now().UTC(),
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	err = db.Create(&crossTenantConversation).Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fk_inbox_conversations_account_tenant")
}

// --- CreateDefaultAdmin ---

func TestCreateDefaultAdmin_CreatesOrgAndUser(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	cfg := &config.DefaultAdminConfig{
		Email:    "test-admin@example.com",
		Password: "testpassword123",
		FullName: "Test Admin",
	}

	err := database.CreateDefaultAdmin(db, cfg)
	require.NoError(t, err)

	// Verify user was created
	var user models.User
	err = db.Where("email = ?", cfg.Email).First(&user).Error
	require.NoError(t, err)
	assert.Equal(t, cfg.FullName, user.FullName)
	assert.True(t, user.IsActive)
	assert.True(t, user.IsSuperAdmin)
	assert.NotEmpty(t, user.PasswordHash)

	// Verify an organization was created
	var org models.Organization
	err = db.First(&org).Error
	require.NoError(t, err)
	assert.Equal(t, "ReReply", org.Name)

	// Verify the user belongs to the organization
	assert.Equal(t, org.ID, user.OrganizationID)
}

func TestCreateDefaultAdmin_Idempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	cfg := &config.DefaultAdminConfig{
		Email:    "idempotent-admin@example.com",
		Password: "pass123",
		FullName: "Idempotent Admin",
	}

	require.NoError(t, database.CreateDefaultAdmin(db, cfg))
	require.NoError(t, database.CreateDefaultAdmin(db, cfg))

	var count int64
	db.Model(&models.User{}).Where("email = ?", cfg.Email).Count(&count)
	assert.Equal(t, int64(1), count, "should not create duplicate admin")
}

func TestCreateDefaultAdmin_SkipsWhenAnyUserExists(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	firstCfg := &config.DefaultAdminConfig{
		Email:    "existing-admin@example.com",
		Password: "pass123",
		FullName: "Existing Admin",
	}
	require.NoError(t, database.CreateDefaultAdmin(db, firstCfg))

	differentCfg := &config.DefaultAdminConfig{
		Email:    "new-default@example.com",
		Password: "pass456",
		FullName: "New Default",
	}
	require.NoError(t, database.CreateDefaultAdmin(db, differentCfg))

	var count int64
	require.NoError(t, db.Unscoped().Model(&models.User{}).Count(&count).Error)
	assert.Equal(t, int64(1), count, "an existing installation must not receive another default admin")
}

func TestCreateDefaultAdmin_SkipsSoftDeletedUser(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	cfg := &config.DefaultAdminConfig{
		Email:    "deleted-admin@example.com",
		Password: "pass123",
		FullName: "Deleted Admin",
	}
	require.NoError(t, database.CreateDefaultAdmin(db, cfg))

	var user models.User
	require.NoError(t, db.Where("email = ?", cfg.Email).First(&user).Error)
	require.NoError(t, db.Delete(&user).Error)
	require.NoError(t, database.CreateDefaultAdmin(db, cfg))

	var unscopedCount int64
	require.NoError(t, db.Unscoped().Model(&models.User{}).Where("email = ?", cfg.Email).Count(&unscopedCount).Error)
	assert.Equal(t, int64(1), unscopedCount, "soft-deleted users must not be recreated by migrations")

	var activeCount int64
	require.NoError(t, db.Model(&models.User{}).Where("email = ?", cfg.Email).Count(&activeCount).Error)
	assert.Zero(t, activeCount)
}

func TestCreateDefaultAdmin_UsesExistingOrg(t *testing.T) {
	db := testutil.SetupTestDB(t)
	cleanAll(t, db)

	// Pre-create an organization
	existingOrg := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "Pre-existing Org",
		Settings:  models.JSONB{},
	}
	require.NoError(t, db.Create(&existingOrg).Error)

	cfg := &config.DefaultAdminConfig{
		Email:    "admin-existing-org@example.com",
		Password: "password",
		FullName: "Admin",
	}

	err := database.CreateDefaultAdmin(db, cfg)
	require.NoError(t, err)

	var user models.User
	err = db.Where("email = ?", cfg.Email).First(&user).Error
	require.NoError(t, err)
	assert.Equal(t, existingOrg.ID, user.OrganizationID, "admin should belong to existing org")

	// Should not have created a new org
	var orgCount int64
	db.Model(&models.Organization{}).Count(&orgCount)
	assert.Equal(t, int64(1), orgCount, "should reuse existing organization")
}
