package database_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// This wrapper belongs only to a fresh test handle/cache. It does not mutate
// model declarations, the fixture owner's Config, or any global naming rule.
type bootstrapCommandEighthColumnNamer struct{ schema.Namer }

func (n bootstrapCommandEighthColumnNamer) ColumnName(table, column string) string {
	if table == "whatsapp_accounts" && column == "Name" {
		return "pr7_mismatched_name"
	}
	return n.Namer.ColumnName(table, column)
}

func TestTenantRLS_CatalogBootstrapCommandEighthPlanRefusesBeforeDDL(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	f := catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, f)
	profile, err := loadProductionProfile()
	require.NoError(t, err)
	require.Len(t, profile.BootstrapColumnOrder, 8)
	require.Equal(t, "whatsapp_accounts", profile.BootstrapColumnOrder[7].Table)

	// A fresh literal Config gives this Open its own schema cache and callback
	// registry. postgres v1.5.6 Apply accepts only its concrete naming strategy;
	// install the wrapper after Open, before this handle parses any model.
	owner, err := gorm.Open(postgres.Open(cfg.Database.MigrationURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(owner)) })
	pool, err := owner.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	owner.NamingStrategy = bootstrapCommandEighthColumnNamer{Namer: owner.NamingStrategy}

	// Establish the fault really is one column of the eighth table. The first
	// seven complete mappings are unchanged, including the number of columns.
	modelsByTable := map[string]*gorm.Statement{}
	for _, model := range database.GetMigrationModels() {
		statement := &gorm.Statement{DB: owner}
		require.NoError(t, statement.Parse(model.Model))
		_, exists := modelsByTable[statement.Schema.Table]
		require.False(t, exists)
		modelsByTable[statement.Schema.Table] = statement
	}
	for index, order := range profile.BootstrapColumnOrder {
		statement, exists := modelsByTable[order.Table]
		require.True(t, exists)
		columns := []string{}
		for _, name := range statement.Schema.DBNames {
			if !statement.Schema.FieldsByDBName[name].IgnoreMigration {
				columns = append(columns, name)
			}
		}
		expected := append([]string(nil), order.Columns...)
		if index == 7 {
			replaced := 0
			for position, name := range expected {
				if name == "name" {
					expected[position] = "pr7_mismatched_name"
					replaced++
				}
			}
			require.Equal(t, 1, replaced)
			require.NotContains(t, statement.Schema.FieldsByDBName, "name")
			require.NotNil(t, statement.Schema.FieldsByName["Name"])
			require.Equal(t, "pr7_mismatched_name", statement.Schema.FieldsByName["Name"].DBName)
		}
		require.ElementsMatch(t, expected, columns, order.Table)
	}
	// Independently prove the ordinary fixture cache was not changed.
	for _, model := range database.GetMigrationModels() {
		statement := &gorm.Statement{DB: f.owner}
		require.NoError(t, statement.Parse(model.Model))
		if statement.Schema.Table == "whatsapp_accounts" {
			require.Contains(t, statement.Schema.FieldsByDBName, "name")
			require.NotContains(t, statement.Schema.FieldsByDBName, "pr7_mismatched_name")
		}
	}

	// Observe, without modifying results, the owned handle's actual Exec path.
	// All public bootstrap DDL in the reviewed candidate uses this Raw chain.
	var mu sync.Mutex
	attemptedDDL := []string{}
	require.NoError(t, owner.Callback().Raw().Before("gorm:raw").Register("pr7:observe_bootstrap_ddl", func(tx *gorm.DB) {
		words := strings.Fields(strings.ToUpper(tx.Statement.SQL.String()))
		if len(words) == 0 {
			return
		}
		switch words[0] {
		case "CREATE", "ALTER", "DROP", "TRUNCATE", "GRANT", "REVOKE", "COMMENT":
			mu.Lock()
			attemptedDDL = append(attemptedDDL, words[0])
			mu.Unlock()
		}
	}))
	before := bootstrapCommandEmptyPublicState(t, f.owner)
	profileRaw, overlayRaw := dbcatalog.BootstrapEmptyProfileV0()
	calls := 0
	err = database.BootstrapEmptyDatabase(owner, f.runtime, cfg, "v0", profileRaw, overlayRaw,
		bootstrapCommandRealBackfills(&calls))
	require.EqualError(t, err, "bootstrap production column identity")
	require.Zero(t, calls)
	mu.Lock()
	ddl := append([]string(nil), attemptedDDL...)
	mu.Unlock()
	require.Empty(t, ddl, "even an attempted public DDL must follow all eight successful plans")
	require.Equal(t, before, bootstrapCommandEmptyPublicState(t, f.owner))
}

func TestTenantRLS_CatalogBootstrapCommandLateCreateFailurePersistsAndRefusesRetry(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	f := catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, f)
	profile, err := loadProductionProfile()
	require.NoError(t, err)
	require.Len(t, profile.BootstrapColumnOrder, 8)
	require.Equal(t, "conversation_reads", profile.BootstrapColumnOrder[0].Table)
	require.Equal(t, "inbound_events", profile.BootstrapColumnOrder[1].Table)
	require.Equal(t, "messages", profile.BootstrapColumnOrder[2].Table)
	known, globals := loadCatalogGoldens(t)
	profileRaw, overlayRaw := dbcatalog.BootstrapEmptyProfileV0()

	// The administrative fixture connects only to this newly owned database.
	// Product owner/runtime roles and grants are not elevated or substituted.
	adminURL := *catalogTemplate.base
	adminURL.Path, adminURL.RawPath = "/"+f.name, ""
	admin, err := catalogConnect(&adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(admin)) })
	var connectedDatabase string
	var superuser bool
	require.NoError(t, admin.Raw("SELECT pg_catalog.current_database()").Scan(&connectedDatabase).Error)
	require.Equal(t, f.name, connectedDatabase)
	require.NoError(t, admin.Raw("SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user").Scan(&superuser).Error)
	require.True(t, superuser, "the isolated event-trigger fixture requires its existing test administrator")
	var preexisting int64
	require.NoError(t, admin.Raw(`SELECT
		(SELECT COUNT(*) FROM pg_catalog.pg_namespace WHERE nspname='pr7_bootstrap_fault') +
		(SELECT COUNT(*) FROM pg_catalog.pg_event_trigger WHERE evtname='pr7_bootstrap_late_create')`).Scan(&preexisting).Error)
	require.Zero(t, preexisting)
	// Register cleanup before any fixture DDL. Both removals are attempted even
	// if one fails; the original fixture still owns final DROP DATABASE cleanup.
	t.Cleanup(func() {
		if err := admin.Exec("DROP EVENT TRIGGER IF EXISTS pr7_bootstrap_late_create").Error; err != nil {
			t.Error("owned bootstrap event-trigger cleanup failed")
		}
		if err := admin.Exec("DROP SCHEMA IF EXISTS pr7_bootstrap_fault CASCADE").Error; err != nil {
			t.Error("owned bootstrap fault-schema cleanup failed")
		}
	})
	require.NoError(t, admin.Exec("CREATE SCHEMA pr7_bootstrap_fault").Error)
	require.NoError(t, admin.Exec(`CREATE FUNCTION pr7_bootstrap_fault.reject_late_create()
		RETURNS event_trigger LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog AS $body$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM pg_catalog.pg_event_trigger_ddl_commands() command
				JOIN pg_catalog.pg_class relation ON command.classid='pg_catalog.pg_class'::regclass AND relation.oid=command.objid
				JOIN pg_catalog.pg_namespace namespace ON namespace.oid=relation.relnamespace
				WHERE command.command_tag='CREATE TABLE' AND command.object_type='table'
					AND namespace.nspname='public' AND relation.relname='messages'
			) THEN
				RAISE EXCEPTION USING ERRCODE='P0001', MESSAGE='pr7 synthetic late create failure';
			END IF;
		END;
		$body$`).Error)
	require.NoError(t, admin.Exec(`CREATE EVENT TRIGGER pr7_bootstrap_late_create
		ON ddl_command_end WHEN TAG IN ('CREATE TABLE')
		EXECUTE FUNCTION pr7_bootstrap_fault.reject_late_create()`).Error)
	bootstrapCommandEmptyPublicState(t, f.owner)
	calls := 0
	err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, "v0", profileRaw, overlayRaw,
		bootstrapCommandRealBackfills(&calls))
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "P0001", pgErr.Code)
	require.Equal(t, "pr7 synthetic late create failure", pgErr.Message)
	require.ErrorContains(t, err, "bootstrap production precreation")
	require.ErrorContains(t, err, "empty bootstrap failed after a persistent phase began; partial state may remain")
	require.Zero(t, calls)

	// An independent connection sees two earlier committed tables; the failed
	// third statement and its table rolled back. No outer transaction is faked.
	var tables []string
	require.NoError(t, admin.Raw(`SELECT c.relname::text FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind='r' ORDER BY c.relname`).Scan(&tables).Error)
	require.Equal(t, []string{"conversation_reads", "inbound_events"}, tables)
	for _, order := range profile.BootstrapColumnOrder[:2] {
		var columns []string
		require.NoError(t, admin.Raw(`SELECT a.attname::text FROM pg_catalog.pg_attribute a
			JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND c.relname=? AND a.attnum>0 AND NOT a.attisdropped
			ORDER BY a.attnum`, order.Table).Scan(&columns).Error)
		require.Equal(t, order.Columns, columns, order.Table)
	}
	// Remove the fault's ability to fire before retry. The next refusal must
	// arise from the real nonempty guard, not from a second injected exception.
	require.NoError(t, admin.Exec("ALTER EVENT TRIGGER pr7_bootstrap_late_create DISABLE").Error)
	readPartial := func() dbcatalog.Snapshot {
		actual, err := dbcatalog.Capture(context.Background(), f.runtimeDSN, dbcatalog.Options{
			Catalog: known, GlobalTables: globals, IncludeSeeds: false, Timeout: 45 * time.Second,
		})
		require.NoError(t, err)
		return actual
	}
	beforeRetry := readPartial()
	err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, "v0", profileRaw, overlayRaw,
		bootstrapCommandRealBackfills(&calls))
	require.EqualError(t, err, "bootstrap public schema is not empty")
	require.Zero(t, calls)
	require.Equal(t, beforeRetry, readPartial())
	var counts []int64
	require.NoError(t, admin.Raw(`SELECT COUNT(*) FROM public.conversation_reads
		UNION ALL SELECT COUNT(*) FROM public.inbound_events`).Scan(&counts).Error)
	require.Equal(t, []int64{0, 0}, counts)
}
