package testutil

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	testDB        *gorm.DB
	testDBOnce    sync.Once
	testDBInitErr error
)

// SetupTestDB creates a connection to a test PostgreSQL database.
// Requires TEST_DATABASE_URL environment variable to be set.
// If not set, the test will be skipped.
// Migrations are run only once across all tests to avoid conflicts.
func SetupTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping database test")
	}

	// Initialize database and run migrations only once
	testDBOnce.Do(func() {
		var err error
		// Several migration regressions deliberately replace an old table shape
		// in the same package process. Disable pgx's implicit named-statement
		// cache so the recreated relation cannot retain the former result type.
		// describe_exec keeps normal extended-protocol value encoding (including
		// JSONB), unlike simple protocol. Production DB configuration is unchanged.
		if parsed, parseErr := url.Parse(dsn); parseErr == nil && parsed.Scheme != "" {
			query := parsed.Query()
			query.Set("statement_cache_capacity", "0")
			query.Set("default_query_exec_mode", "describe_exec")
			parsed.RawQuery = query.Encode()
			dsn = parsed.String()
		}
		testDB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			testDBInitErr = fmt.Errorf("failed to connect to test postgres: %w", err)
			return
		}

		// Run migrations once
		if err := runMigrations(testDB); err != nil {
			testDBInitErr = fmt.Errorf("failed to run migrations: %w", err)
			return
		}

		// Clean up any existing data before tests start
		cleanupTables(testDB)
	})

	if testDBInitErr != nil {
		t.Fatalf("failed to initialize test database: %v", testDBInitErr)
	}

	// Return a new session for this test to avoid connection conflicts
	return testDB.Session(&gorm.Session{})
}

// OpenTestDBAsRole opens a direct test connection whose session_user and
// current_user are both the supplied login role. It intentionally runs no
// migrations and is used for startup-verifier identity tests that cannot be
// faithfully exercised with SET ROLE.
func OpenTestDBAsRole(t *testing.T, role, password string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Scheme, "TEST_DATABASE_URL must be a PostgreSQL URL")
	parsed.User = url.UserPassword(role, password)
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

// OpenIsolatedTestDatabaseOwnedByRole creates a disposable database whose
// direct LOGIN owner is deliberately NOSUPERUSER. It is used for migration
// authority tests that cannot be represented faithfully by SET ROLE or by the
// superuser-owned shared CI database. It owns and removes both the disposable
// database and its cluster-global runtime/owner roles in one cleanup.
func OpenIsolatedTestDatabaseOwnedByRole(t *testing.T) (*gorm.DB, *gorm.DB, string, string) {
	t.Helper()
	clusterDB := SetupTestDB(t)
	baseURL, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.NotEmpty(t, baseURL.Scheme, "TEST_DATABASE_URL must be a PostgreSQL URL")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	ownerRole := "rereply_migration_" + suffix[:16]
	runtimeRole := "rereply_compliance_test_" + suffix[:16]
	databaseName := "rereply_compliance_" + suffix
	ownerPassword := uuid.NewString() + uuid.NewString()

	ownerRoleCreated := false
	runtimeRoleCreated := false
	databaseCreated := false
	var ownerSQLDB interface{ Close() error }
	var isolatedAdminSQLDB interface{ Close() error }
	t.Cleanup(func() {
		if ownerSQLDB != nil {
			if closeErr := ownerSQLDB.Close(); closeErr != nil {
				t.Errorf("close isolated migration-owner database: %v", closeErr)
			}
		}
		if isolatedAdminSQLDB != nil {
			if closeErr := isolatedAdminSQLDB.Close(); closeErr != nil {
				t.Errorf("close isolated administrator database: %v", closeErr)
			}
		}
		if databaseCreated {
			if dropErr := clusterDB.Exec("DROP DATABASE IF EXISTS " + databaseName + " WITH (FORCE)").Error; dropErr != nil {
				t.Errorf("drop isolated migration-owner database: %v", dropErr)
			}
		}
		if runtimeRoleCreated {
			if dropErr := clusterDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error; dropErr != nil {
				t.Errorf("drop isolated runtime role: %v", dropErr)
			}
		}
		if ownerRoleCreated {
			if dropErr := clusterDB.Exec("DROP ROLE IF EXISTS " + ownerRole).Error; dropErr != nil {
				t.Errorf("drop isolated migration-owner role: %v", dropErr)
			}
		}
	})

	require.NoError(t, clusterDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
		ownerRole,
		quoteTestPostgresLiteral(ownerPassword),
	)).Error)
	ownerRoleCreated = true
	require.NoError(t, clusterDB.Exec(
		"CREATE ROLE "+runtimeRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
	).Error)
	runtimeRoleCreated = true
	require.NoError(t, clusterDB.Exec(
		"CREATE DATABASE "+databaseName+" OWNER "+ownerRole,
	).Error)
	databaseCreated = true

	baseURL.Path = "/" + databaseName
	baseURL.RawPath = ""
	query := baseURL.Query()
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	baseURL.RawQuery = query.Encode()
	isolatedAdminDB, err := gorm.Open(postgres.Open(baseURL.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	isolatedAdminSQLDB, err = isolatedAdminDB.DB()
	require.NoError(t, err)

	ownerURL := *baseURL
	ownerURL.User = url.UserPassword(ownerRole, ownerPassword)
	ownerDB, err := gorm.Open(postgres.Open(ownerURL.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := ownerDB.DB()
	require.NoError(t, err)
	ownerSQLDB = sqlDB
	require.NoError(t, runMigrations(ownerDB))
	return ownerDB, isolatedAdminDB, ownerRole, runtimeRole
}

// OpenIsolatedTestDatabaseAsAdmin creates a disposable database using the
// TEST_DATABASE_URL login itself. It exists only for negative authority tests
// that must prove a table-owning superuser is rejected.
func OpenIsolatedTestDatabaseAsAdmin(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	clusterDB := SetupTestDB(t)
	baseURL, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.NotEmpty(t, baseURL.Scheme, "TEST_DATABASE_URL must be a PostgreSQL URL")

	var superuser bool
	require.NoError(t, clusterDB.Raw(`
		SELECT role.rolsuper
		FROM pg_catalog.pg_roles AS role
		WHERE role.rolname = current_user
	`).Scan(&superuser).Error)
	if !superuser {
		t.Skip("negative migration-authority regression requires a disposable superuser test login")
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	databaseName := "rereply_superuser_" + suffix
	runtimeRole := "rereply_compliance_runtime_" + suffix[:16]
	require.NoError(t, clusterDB.Exec("CREATE DATABASE "+databaseName).Error)
	databaseCreated := true
	runtimeRoleCreated := false
	var isolatedSQLDB interface{ Close() error }
	t.Cleanup(func() {
		if isolatedSQLDB != nil {
			if closeErr := isolatedSQLDB.Close(); closeErr != nil {
				t.Errorf("close isolated superuser database: %v", closeErr)
			}
		}
		if databaseCreated {
			if dropErr := clusterDB.Exec("DROP DATABASE IF EXISTS " + databaseName + " WITH (FORCE)").Error; dropErr != nil {
				t.Errorf("drop isolated superuser database: %v", dropErr)
			}
		}
		if runtimeRoleCreated {
			if dropErr := clusterDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error; dropErr != nil {
				t.Errorf("drop isolated superuser-test runtime role: %v", dropErr)
			}
		}
	})
	require.NoError(t, clusterDB.Exec(
		"CREATE ROLE "+runtimeRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
	).Error)
	runtimeRoleCreated = true

	baseURL.Path = "/" + databaseName
	baseURL.RawPath = ""
	query := baseURL.Query()
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	baseURL.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(baseURL.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	isolatedSQLDB = sqlDB
	require.NoError(t, runMigrations(db))
	return db, runtimeRole
}

func quoteTestPostgresLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// SetupTestDBWithCleanup is like SetupTestDB but allows controlling cleanup behavior.
func SetupTestDBWithCleanup(t *testing.T, cleanup bool) *gorm.DB {
	t.Helper()

	db := SetupTestDB(t)

	if cleanup {
		t.Cleanup(func() {
			// Clean up only the data created by this test
			// Note: In parallel tests, this may affect other tests
			// Consider using unique identifiers instead
		})
	}

	return db
}

// RequirePostgresBackendWaitingForLock waits until PostgreSQL confirms that a
// known backend is blocked on a lock. It makes transaction-serialization tests
// deterministic instead of treating an elapsed sleep as proof that the
// competing statement reached its lock.
func RequirePostgresBackendWaitingForLock(
	t *testing.T,
	db *gorm.DB,
	backendPID int,
) {
	t.Helper()
	if db == nil || backendPID <= 0 {
		t.Fatalf("valid database and PostgreSQL backend PID are required")
	}

	type backendActivity struct {
		State         string `gorm:"column:state"`
		WaitEventType string `gorm:"column:wait_event_type"`
		WaitEvent     string `gorm:"column:wait_event"`
	}
	deadline := time.Now().Add(5 * time.Second)
	var last backendActivity
	var lastErr error
	for time.Now().Before(deadline) {
		last = backendActivity{}
		lastErr = db.Raw(`
			SELECT
				COALESCE(state, '') AS state,
				COALESCE(wait_event_type, '') AS wait_event_type,
				COALESCE(wait_event, '') AS wait_event
			FROM pg_catalog.pg_stat_activity
			WHERE pid = ?
		`, backendPID).Scan(&last).Error
		if lastErr == nil && last.WaitEventType == "Lock" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf(
		"PostgreSQL backend %d did not wait for a lock (state=%q wait_type=%q wait=%q error=%v)",
		backendPID,
		last.State,
		last.WaitEventType,
		last.WaitEvent,
		lastErr,
	)
}

// runMigrations runs all model migrations.
func runMigrations(db *gorm.DB) error {
	migrations := database.GetMigrationModels()
	modelsToMigrate := make([]any, 0, len(migrations)+1)
	for _, migration := range migrations {
		modelsToMigrate = append(modelsToMigrate, migration.Model)
	}
	// Retain the legacy step table in tests while the graph backfill path still
	// exercises it. Production intentionally no longer creates this table.
	modelsToMigrate = append(modelsToMigrate, &models.ChatbotFlowStep{})
	// Mirror the production pre-AutoMigrate ownership upgrade. A shared local
	// test database may retain the unreleased nullable journal shape after an
	// interrupted earlier process.
	if err := database.PrepareMetaInstagramDeletionJournalTenant(db); err != nil {
		return err
	}
	if err := database.PrepareProviderIntegrationManagementMode(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(modelsToMigrate...); err != nil {
		return err
	}
	// The shared local PostgreSQL container can contain rows from an interrupted
	// prior test process. Clear disposable test data before applying production
	// unique indexes so stale fixtures cannot block schema verification.
	cleanupTables(db)
	return database.CreateIndexes(db)
}

// cleanupTables removes all data from tables (for PostgreSQL cleanup).
// Uses TRUNCATE CASCADE to handle foreign key constraints properly.
func cleanupTables(db *gorm.DB) {
	truncateModelTables(db)
}

// TruncateTables truncates all tables (PostgreSQL only, faster than DELETE).
func TruncateTables(db *gorm.DB) {
	truncateModelTables(db)
}

// truncateModelTables derives the cleanup list from the production migration
// registry so newly introduced product tables cannot silently retain test data.
func truncateModelTables(db *gorm.DB) {
	tables := make(map[string]struct{})
	for _, migration := range database.GetMigrationModels() {
		statement := &gorm.Statement{DB: db}
		if err := statement.Parse(migration.Model); err == nil && statement.Schema != nil {
			tables[statement.Schema.Table] = struct{}{}
		}
	}
	tables["chatbot_flow_steps"] = struct{}{}
	tables["role_permissions"] = struct{}{}

	ordered := make([]string, 0, len(tables)+1)
	for table := range tables {
		ordered = append(ordered, table)
	}
	sort.Strings(ordered)

	if err := db.Transaction(func(tx *gorm.DB) error {
		var truncateGuardInstalled bool
		if err := tx.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
				  AND trigger.tgname = 'rereply_platform_compliance_truncate_guard'
				  AND NOT trigger.tgisinternal
			)
		`).Scan(&truncateGuardInstalled).Error; err != nil {
			return fmt.Errorf("inspect organization truncate guard: %w", err)
		}
		if truncateGuardInstalled {
			if err := tx.Exec(`
				ALTER TABLE public.organizations
				DISABLE TRIGGER rereply_platform_compliance_truncate_guard
			`).Error; err != nil {
				return fmt.Errorf("disable test organization truncate guard: %w", err)
			}
		}
		var identityTruncateGuardInstalled bool
		if err := tx.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'organization_identity_registry'
				  AND trigger.tgname = 'rereply_platform_compliance_identity_registry_truncate_guard'
				  AND NOT trigger.tgisinternal
			)
		`).Scan(&identityTruncateGuardInstalled).Error; err != nil {
			return fmt.Errorf("inspect organization identity registry truncate guard: %w", err)
		}
		if identityTruncateGuardInstalled {
			if err := tx.Exec(`
				ALTER TABLE public.organization_identity_registry
				DISABLE TRIGGER rereply_platform_compliance_identity_registry_truncate_guard
			`).Error; err != nil {
				return fmt.Errorf("disable test organization identity registry truncate guard: %w", err)
			}
		}
		for _, table := range ordered {
			quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
			if err := tx.Exec(fmt.Sprintf("TRUNCATE TABLE public.%s CASCADE", quoted)).Error; err != nil {
				return fmt.Errorf("truncate test table %s: %w", table, err)
			}
		}
		var identityRegistryExists bool
		if err := tx.Raw(`
			SELECT pg_catalog.to_regclass('public.organization_identity_registry') IS NOT NULL
		`).Scan(&identityRegistryExists).Error; err != nil {
			return fmt.Errorf("inspect organization identity registry: %w", err)
		}
		if identityRegistryExists {
			if err := tx.Exec("TRUNCATE TABLE public.organization_identity_registry").Error; err != nil {
				return fmt.Errorf("truncate test organization identity registry: %w", err)
			}
		}
		if truncateGuardInstalled {
			if err := tx.Exec(`
				ALTER TABLE public.organizations
				ENABLE TRIGGER rereply_platform_compliance_truncate_guard
			`).Error; err != nil {
				return fmt.Errorf("re-enable test organization truncate guard: %w", err)
			}
		}
		if identityTruncateGuardInstalled {
			if err := tx.Exec(`
				ALTER TABLE public.organization_identity_registry
				ENABLE TRIGGER rereply_platform_compliance_identity_registry_truncate_guard
			`).Error; err != nil {
				return fmt.Errorf("re-enable test organization identity registry truncate guard: %w", err)
			}
		}
		return nil
	}); err != nil {
		panic(fmt.Sprintf("truncate disposable PostgreSQL test tables: %v", err))
	}
	var disabledUserTriggers int64
	if err := db.Raw(`
		SELECT pg_catalog.count(*)
		FROM pg_catalog.pg_trigger AS trigger
		JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public'
		  AND NOT trigger.tgisinternal
		  AND trigger.tgenabled <> 'O'
	`).Scan(&disabledUserTriggers).Error; err != nil {
		panic(fmt.Sprintf("verify disposable PostgreSQL test trigger cleanup: %v", err))
	}
	if disabledUserTriggers != 0 {
		panic(fmt.Sprintf(
			"verify disposable PostgreSQL test trigger cleanup: %d public user triggers remain disabled",
			disabledUserTriggers,
		))
	}
}
