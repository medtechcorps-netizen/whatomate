package database_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func bootstrapCommandFixtureConfig(t *testing.T, f catalogFixture) *config.Config {
	t.Helper()
	for _, db := range []*gorm.DB{f.owner, f.runtime} {
		pool, err := db.DB()
		require.NoError(t, err)
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
	}
	return &config.Config{
		App: config.AppConfig{Environment: "test"},
		Database: config.DatabaseConfig{
			URL: f.runtimeDSN,
			MigrationURL: catalogRoleURL(catalogTemplate.base, f.name,
				catalogTemplate.owner, catalogTemplate.ownerPassword).String(),
			RuntimeRole: catalogTemplate.runtime,
			RLSEnabled:  true,
		},
		DefaultAdmin: config.DefaultAdminConfig{
			Email:    "bootstrap-command@example.invalid",
			Password: "synthetic_bootstrap_command_password_only",
			FullName: "Synthetic Bootstrap Command",
		},
	}
}

func bootstrapCommandRealBackfills(calls *int) func(*gorm.DB) error {
	return func(session *gorm.DB) error {
		(*calls)++
		lo := logf.New(logf.Opts{Writer: io.Discard})
		if err := handlers.BackfillChatbotFlowGraph(session, lo); err != nil {
			return err
		}
		_, err := channel.BackfillLegacyWhatsAppInbox(session, 500)
		return err
	}
}

// The expected complete catalog is built only from the committed golden and
// reviewed production delta. No candidate database values supply its oracle.
func bootstrapCommandOracle(t *testing.T, shape string) (dbcatalog.Catalog, []dbcatalog.GlobalTable, []byte, string) {
	t.Helper()
	known, globals := loadCatalogGoldens(t)
	profile, err := loadProductionProfile()
	require.NoError(t, err)
	expected := dbcatalog.Catalog{Version: known.Version, Objects: make([]dbcatalog.Object, len(known.Objects))}
	positions := map[string]int{}
	for index, object := range known.Objects {
		expected.Objects[index] = object
		expected.Objects[index].Attributes = map[string]string{}
		for key, value := range object.Attributes {
			expected.Objects[index].Attributes[key] = value
		}
		positions[object.Kind+":"+object.Identity] = index
	}
	wantHash := profile.GoldenSHA256
	if shape == "v0" {
		wantHash = profile.LiveSHA256
		for _, delta := range profile.Delta {
			index, exists := positions[delta.Kind+":"+delta.Identity]
			require.True(t, exists, "reviewed delta must name a committed object")
			for key, change := range delta.Attributes {
				value, exists := expected.Objects[index].Attributes[key]
				require.True(t, exists, "reviewed delta must name an existing attribute")
				require.Equal(t, change.Golden, value)
				expected.Objects[index].Attributes[key] = change.Production
			}
		}
	} else {
		require.Empty(t, shape)
	}
	raw, err := dbcatalog.CanonicalCatalog(expected)
	require.NoError(t, err)
	require.Equal(t, wantHash, fmt.Sprintf("%x", sha256.Sum256(raw)), "independent full oracle must have its reviewed hash")
	if shape == "" {
		original, err := os.ReadFile(filepath.Join(catalogGoldenDirectory, "catalog.json"))
		require.NoError(t, err)
		require.Equal(t, original, raw)
	}
	return known, globals, raw, wantHash
}

func bootstrapCommandAdminAndBusinessState(t *testing.T, owner *gorm.DB, admin config.DefaultAdminConfig) {
	t.Helper()
	var users []models.User
	require.NoError(t, owner.Unscoped().Find(&users).Error)
	require.Len(t, users, 1)
	user := users[0]
	require.Equal(t, admin.Email, user.Email)
	require.Equal(t, admin.FullName, user.FullName)
	require.True(t, user.IsActive)
	require.True(t, user.IsSuperAdmin)
	require.False(t, user.DeletedAt.Valid)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(admin.Password)))
	require.NotNil(t, user.RoleID)
	var role models.CustomRole
	require.NoError(t, owner.First(&role, "id = ?", *user.RoleID).Error)
	require.Equal(t, "admin", role.Name)
	require.True(t, role.IsSystem)
	require.Equal(t, user.OrganizationID, role.OrganizationID)
	var memberships []models.UserOrganization
	require.NoError(t, owner.Unscoped().Where("user_id = ?", user.ID).Find(&memberships).Error)
	require.Len(t, memberships, 1)
	require.True(t, memberships[0].IsDefault)
	require.Equal(t, user.OrganizationID, memberships[0].OrganizationID)
	require.Equal(t, user.RoleID, memberships[0].RoleID)
	var contacts, messages int64
	require.NoError(t, owner.Unscoped().Model(&models.Contact{}).Count(&contacts).Error)
	require.NoError(t, owner.Unscoped().Model(&models.Message{}).Count(&messages).Error)
	require.Zero(t, contacts)
	require.Zero(t, messages)
}

// Hash complete rows in every independently known ordinary table. This is a
// same-database before/after nonmutation check, not a catalog/seed oracle.
func bootstrapCommandRows(t *testing.T, owner *gorm.DB, known dbcatalog.Catalog) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, object := range known.Objects {
		if object.Kind != "relations" || object.Attributes["relkind"] != "r" {
			continue
		}
		name := `"` + strings.ReplaceAll(object.Identity, `"`, `""`) + `"`
		var raw string
		read := owner.Raw(`SELECT COALESCE(jsonb_agg(to_jsonb(source) ORDER BY to_jsonb(source)::text), '[]'::jsonb)::text FROM public.` + name + ` AS source`).Scan(&raw)
		require.NoError(t, read.Error)
		require.EqualValues(t, 1, read.RowsAffected)
		result[object.Identity] = fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	}
	require.NotEmpty(t, result)
	return result
}

func TestTenantRLS_CatalogBootstrapCommandNormalAndV0(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	for _, shape := range []string{"", "v0"} {
		name := shape
		if name == "" {
			name = "normal"
		}
		t.Run(name, func(t *testing.T) {
			known, globals, expectedRaw, expectedHash := bootstrapCommandOracle(t, shape)
			f := catalogEmptyTarget(t)
			cfg := bootstrapCommandFixtureConfig(t, f)
			var profileRaw, overlayRaw []byte
			if shape == "v0" {
				profileRaw, overlayRaw = dbcatalog.BootstrapEmptyProfileV0()
			}
			calls := 0
			require.NoError(t, database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, shape,
				profileRaw, overlayRaw, bootstrapCommandRealBackfills(&calls)))
			require.Equal(t, 1, calls)
			// This is an additional independent postcondition; the entrypoint's
			// internal concrete runtime verification remains unchanged and real.
			require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
			actual := captureSyntheticCatalog(t, f, known, globals)
			require.Empty(t, actual.Unknown)
			for _, object := range actual.Objects {
				require.False(t, object.Missing, object.Identity)
			}
			require.True(t, actual.Ownership.MigrationOwnerOwnsDatabase)
			require.True(t, actual.Ownership.MigrationOwnerOwnsPublicSchema)
			actualRaw, err := dbcatalog.CanonicalCatalog(dbcatalog.Catalog{Version: 0, Objects: actual.Objects})
			require.NoError(t, err)
			require.Equal(t, expectedRaw, actualRaw)
			require.Equal(t, expectedHash, actual.SHA256)
			require.Equal(t, []dbcatalog.Seed{sourcePermissionSeed(t, f.runtime)}, actual.Seeds)
			bootstrapCommandAdminAndBusinessState(t, f.owner, cfg.DefaultAdmin)
			if shape == "v0" {
				profile, err := loadProductionProfile()
				require.NoError(t, err)
				require.Len(t, profile.BootstrapColumnOrder, 8)
				for _, order := range profile.BootstrapColumnOrder {
					var columns []string
					require.NoError(t, f.owner.Raw(`SELECT a.attname::text
						FROM pg_catalog.pg_attribute a
						JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
						JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
						WHERE n.nspname='public' AND c.relname=? AND a.attnum>0 AND NOT a.attisdropped
						ORDER BY a.attnum`, order.Table).Scan(&columns).Error)
					require.Equal(t, order.Columns, columns, order.Table)
				}
			}
			beforeRows := bootstrapCommandRows(t, f.owner, known)
			err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, shape,
				profileRaw, overlayRaw, bootstrapCommandRealBackfills(&calls))
			require.ErrorContains(t, err, "bootstrap public schema is not empty")
			require.Equal(t, 1, calls, "second invocation must not reach real backfills")
			after := captureSyntheticCatalog(t, f, known, globals)
			require.Equal(t, actual, after)
			require.Equal(t, beforeRows, bootstrapCommandRows(t, f.owner, known))
			require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
		})
	}
}

func bootstrapCommandEmptyPublicState(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var rows []struct {
		State   string `gorm:"column:state"`
		Objects int64  `gorm:"column:objects"`
	}
	read := db.Raw(`WITH counts AS (SELECT
		(SELECT COUNT(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public') AS relations,
		(SELECT COUNT(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public') AS functions,
		(SELECT COUNT(*) FROM pg_catalog.pg_type ty JOIN pg_catalog.pg_namespace n ON n.oid=ty.typnamespace WHERE n.nspname='public') AS types,
		(SELECT COUNT(*) FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_namespace n ON n.oid=d.refobjid WHERE d.refclassid='pg_catalog.pg_namespace'::regclass AND n.nspname='public') AS dependencies)
		SELECT jsonb_build_object('namespace',n.oid,'owner',n.nspowner,'acl',COALESCE(n.nspacl::text,''),'counts',to_jsonb(counts))::text AS state,
			relations+functions+types+dependencies AS objects
		FROM counts CROSS JOIN pg_catalog.pg_namespace n WHERE n.nspname='public'`).Scan(&rows)
	require.NoError(t, read.Error)
	require.EqualValues(t, 1, read.RowsAffected)
	require.Len(t, rows, 1)
	require.Zero(t, rows[0].Objects)
	return rows[0].State
}

func TestTenantRLS_CatalogBootstrapCommandRejectsDifferentDatabaseBeforeWrites(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	ownerTarget, runtimeTarget := catalogEmptyTarget(t), catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, ownerTarget)
	bootstrapCommandFixtureConfig(t, runtimeTarget)
	cfg.Database.URL = runtimeTarget.runtimeDSN
	var ownerDatabase, runtimeDatabase string
	var ownerOID, runtimeOID int64
	require.NoError(t, ownerTarget.owner.Raw("SELECT pg_catalog.current_database()").Scan(&ownerDatabase).Error)
	require.NoError(t, runtimeTarget.runtime.Raw("SELECT pg_catalog.current_database()").Scan(&runtimeDatabase).Error)
	require.NoError(t, ownerTarget.owner.Raw("SELECT oid::bigint FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database()").Scan(&ownerOID).Error)
	require.NoError(t, runtimeTarget.runtime.Raw("SELECT oid::bigint FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database()").Scan(&runtimeOID).Error)
	require.Equal(t, ownerTarget.name, ownerDatabase)
	require.Equal(t, runtimeTarget.name, runtimeDatabase)
	require.NotEqual(t, ownerDatabase, runtimeDatabase)
	require.Positive(t, ownerOID)
	require.Positive(t, runtimeOID)
	require.NotEqual(t, ownerOID, runtimeOID)
	beforeOwner := bootstrapCommandEmptyPublicState(t, ownerTarget.owner)
	beforeRuntime := bootstrapCommandEmptyPublicState(t, runtimeTarget.owner)
	calls := 0
	err := database.BootstrapEmptyDatabase(ownerTarget.owner, runtimeTarget.runtime, cfg, "", nil, nil,
		bootstrapCommandRealBackfills(&calls))
	require.EqualError(t, err, "bootstrap paired identity mismatch")
	require.Zero(t, calls)
	require.Equal(t, beforeOwner, bootstrapCommandEmptyPublicState(t, ownerTarget.owner))
	require.Equal(t, beforeRuntime, bootstrapCommandEmptyPublicState(t, runtimeTarget.owner))
}
