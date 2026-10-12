package database_test

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Preserve catalogClone's ordinary absent-DB skip convention. Qualification
// requires a real disposable PG17 run with zero skipped test events.
func catalogLegacyAdminClone(t *testing.T) catalogFixture {
	t.Helper()
	f := catalogClone(t)
	var version int
	require.NoError(t, f.owner.Raw("SELECT current_setting('server_version_num')::integer").Scan(&version).Error)
	require.GreaterOrEqual(t, version, 170000)
	require.Less(t, version, 180000)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
	return f
}

type catalogLegacyAdminState struct {
	User         models.User
	Memberships  []models.UserOrganization
	Role         models.CustomRole
	Organization models.Organization
}

// Capture all selected persisted business fields, including deleted_at and
// permission identities. Ignore only created_at/updated_at bookkeeping; do not
// infer correctness from an unchanged super-admin flag alone.
func catalogLegacyAdminStateFor(t *testing.T, db *gorm.DB, id uuid.UUID) catalogLegacyAdminState {
	t.Helper()
	var state catalogLegacyAdminState
	require.NoError(t, db.Unscoped().First(&state.User, "id = ?", id).Error)
	require.NotNil(t, state.User.RoleID)
	require.NoError(t, db.Unscoped().Where("user_id = ?", id).Order("id").Find(&state.Memberships).Error)
	require.Len(t, state.Memberships, 1)
	membership := state.Memberships[0]
	require.Equal(t, state.User.OrganizationID, membership.OrganizationID)
	require.Equal(t, state.User.RoleID, membership.RoleID)
	require.True(t, membership.IsDefault)
	require.Equal(t, "direct", membership.Source)
	require.False(t, membership.DeletedAt.Valid)
	require.NoError(t, db.Unscoped().First(&state.Role, "id = ?", *state.User.RoleID).Error)
	require.Equal(t, state.User.OrganizationID, state.Role.OrganizationID)
	require.True(t, state.Role.IsSystem)
	require.False(t, state.Role.DeletedAt.Valid)
	var permissions []models.Permission
	require.NoError(t, db.Model(&state.Role).Association("Permissions").Find(&permissions))
	require.NotEmpty(t, permissions)
	sort.Slice(permissions, func(i, j int) bool {
		if permissions[i].Resource == permissions[j].Resource {
			return permissions[i].Action < permissions[j].Action
		}
		return permissions[i].Resource < permissions[j].Resource
	})
	for i := range permissions {
		permissions[i].CreatedAt = time.Time{}
		permissions[i].UpdatedAt = time.Time{}
	}
	state.Role.Permissions = permissions
	require.NoError(t, db.Unscoped().First(&state.Organization, "id = ?", state.User.OrganizationID).Error)
	require.False(t, state.Organization.DeletedAt.Valid)
	for _, base := range []*models.BaseModel{&state.User.BaseModel, &state.Role.BaseModel, &state.Organization.BaseModel} {
		base.CreatedAt = time.Time{}
		base.UpdatedAt = time.Time{}
	}
	for i := range state.Memberships {
		state.Memberships[i].CreatedAt = time.Time{}
		state.Memberships[i].UpdatedAt = time.Time{}
	}
	return state
}

func catalogLegacyAdminCounts(t *testing.T, db *gorm.DB) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64)
	for _, table := range []string{"users", "user_organizations", "organizations", "custom_roles", "permissions", "role_permissions"} {
		var count int64
		require.NoError(t, db.Unscoped().Table(table).Count(&count).Error)
		counts[table] = count
	}
	return counts
}

func catalogLegacyAdminUser(t *testing.T, db *gorm.DB, orgID, roleID uuid.UUID, email string, superAdmin bool) models.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic_legacy_regression_only"), bcrypt.MinCost)
	require.NoError(t, err)
	user := models.User{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Email:          email,
		PasswordHash:   string(hash),
		FullName:       "Synthetic legacy regression",
		RoleID:         &roleID,
		Settings:       models.JSONB{"synthetic_case": "legacy-admin-removal"},
		IsActive:       true,
		IsAvailable:    true,
		IsSuperAdmin:   superAdmin,
	}
	require.NoError(t, db.Create(&user).Error)
	membership := models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         user.ID,
		OrganizationID: orgID,
		RoleID:         &roleID,
		IsDefault:      true,
		Source:         "direct",
	}
	require.NoError(t, db.Create(&membership).Error)
	return user
}

func TestTenantRLS_CatalogLegacyAdminSeedDoesNotEscalate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		softDeleted bool
		superAdmin  bool
	}{
		{name: "ordinary"},
		{name: "soft_deleted", softDeleted: true},
		{name: "initially_super_admin", superAdmin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// users.email is globally unique; each state needs its own clone.
			f := catalogLegacyAdminClone(t)
			var bootstrap models.User
			require.NoError(t, f.owner.Where("email = ?", "catalog@example.invalid").First(&bootstrap).Error)
			require.True(t, bootstrap.IsSuperAdmin)
			require.True(t, bootstrap.IsActive)
			var agent models.CustomRole
			require.NoError(t, f.owner.Where("organization_id = ? AND name = ? AND is_system = ?", bootstrap.OrganizationID, "agent", true).First(&agent).Error)
			legacy := catalogLegacyAdminUser(t, f.owner, bootstrap.OrganizationID, agent.ID, "admin@admin.com", tc.superAdmin)
			ordinary := catalogLegacyAdminUser(t, f.owner, bootstrap.OrganizationID, agent.ID, "unrelated@example.invalid", false)
			if tc.softDeleted {
				require.NoError(t, f.owner.Delete(&legacy).Error)
			}
			before := make(map[uuid.UUID]catalogLegacyAdminState)
			for _, id := range []uuid.UUID{legacy.ID, ordinary.ID, bootstrap.ID} {
				before[id] = catalogLegacyAdminStateFor(t, f.owner, id)
			}
			require.Equal(t, tc.superAdmin, before[legacy.ID].User.IsSuperAdmin)
			require.Equal(t, tc.softDeleted, before[legacy.ID].User.DeletedAt.Valid)
			require.False(t, before[ordinary.ID].User.IsSuperAdmin)
			require.True(t, before[bootstrap.ID].User.IsSuperAdmin)
			counts := catalogLegacyAdminCounts(t, f.owner)
			for attempt := 1; attempt <= 2; attempt++ {
				require.NoError(t, database.SeedSystemRolesForAllOrgs(f.owner), "seed attempt %d", attempt)
				for _, id := range []uuid.UUID{legacy.ID, ordinary.ID, bootstrap.ID} {
					after := catalogLegacyAdminStateFor(t, f.owner, id)
					require.Equal(t, before[id], after, "seed attempt %d changed selected user, role, membership, permissions or organization", attempt)
				}
				require.Equal(t, counts, catalogLegacyAdminCounts(t, f.owner), "seed attempt %d changed row counts", attempt)
				var visible int64
				require.NoError(t, f.owner.Model(&models.User{}).Where("id = ?", legacy.ID).Count(&visible).Error)
				if tc.softDeleted {
					require.Zero(t, visible, "seeding must not revive a soft-deleted legacy user")
				} else {
					require.Equal(t, int64(1), visible)
				}
				require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
			}
		})
	}
}

func TestTenantRLS_CatalogDefaultAdminBootstrapPreserved(t *testing.T) {
	f := catalogLegacyAdminClone(t)
	cfg := config.DefaultAdminConfig{
		Email:    "catalog@example.invalid",
		Password: "synthetic_catalog_password_only",
		FullName: "Synthetic Catalog",
	}
	var user models.User
	require.NoError(t, f.owner.Where("email = ?", cfg.Email).First(&user).Error)
	before := catalogLegacyAdminStateFor(t, f.owner, user.ID)
	require.True(t, before.User.IsActive)
	require.True(t, before.User.IsSuperAdmin)
	require.False(t, before.User.DeletedAt.Valid)
	require.Equal(t, cfg.FullName, before.User.FullName)
	require.Equal(t, "admin", before.Role.Name)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(before.User.PasswordHash), []byte(cfg.Password)))
	counts := catalogLegacyAdminCounts(t, f.owner)
	for attempt := 1; attempt <= 2; attempt++ {
		require.NoError(t, database.CreateDefaultAdmin(f.owner, &cfg))
		after := catalogLegacyAdminStateFor(t, f.owner, user.ID)
		require.Equal(t, before, after, "configured bootstrap retry %d changed retained identities or permissions", attempt)
		require.Equal(t, counts, catalogLegacyAdminCounts(t, f.owner))
		require.NoError(t, bcrypt.CompareHashAndPassword([]byte(after.User.PasswordHash), []byte(cfg.Password)))
		require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
	}
}
