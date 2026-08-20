package testutil

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
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

	for table := range tables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		db.Exec(fmt.Sprintf("TRUNCATE TABLE %s CASCADE", quoted))
	}
}
