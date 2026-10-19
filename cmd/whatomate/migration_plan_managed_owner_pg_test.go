package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// This regression uses only the existing loopback-only, generated PG17 fixture.
// It changes the generated owner, never the role/database in TEST_DATABASE_URL.
// Both observers must still reject incomplete facts; the test expects real
// successful observation, not a translated or suppressed error.
func TestMigrationPlanPGManagedOwnerObserverRegression(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	for _, scenario := range []struct {
		name        string
		managed     bool
		replication bool
	}{
		{name: "restricted_owner_control"},
		{name: "managed_owner", managed: true},
		// DigitalOcean documents REPLICATION on its managed owner. This
		// generated role tests that shape without claiming live role facts.
		{name: "managed_owner_with_replication", managed: true, replication: true},
	} {
		if scenario.managed {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			attributes := "CREATEDB CREATEROLE BYPASSRLS NOREPLICATION"
			if scenario.replication {
				attributes = "CREATEDB CREATEROLE BYPASSRLS REPLICATION"
			}
			_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" "+attributes)
			cancel()
			planPGOK(t, err, "configure generated managed owner")
		}
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var super, createRole, bypass, replication, ownsDatabase bool
			err := f.admin.QueryRow(ctx, `SELECT r.rolsuper,r.rolcreaterole,r.rolbypassrls,r.rolreplication,d.datdba=r.oid
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=$2 WHERE r.rolname=$1`, f.owner, f.database).Scan(&super, &createRole, &bypass, &replication, &ownsDatabase)
			planPGOK(t, err, "verify generated owner attributes")
			require.False(t, super)
			require.Equal(t, scenario.replication, replication)
			require.True(t, ownsDatabase)
			require.Equal(t, scenario.managed, createRole)
			require.Equal(t, scenario.managed, bypass)
			t.Run("data_gates", func(t *testing.T) { managedOwnerDataGateProbe(t, f) })
			t.Run("planner", func(t *testing.T) { managedOwnerPlannerProbe(t, f) })
		})
	}
}

func managedOwnerRows(t *testing.T, f *migrationPlanFixture) map[string][32]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(map[string][32]byte)
	for _, table := range []string{"contacts", "messages", "scheduled_jobs", "permissions", "custom_roles", "role_permissions", "organizations", "users", "resellers", "reseller_members", "plans", "plan_prices", "plan_entitlements", "widgets"} {
		var raw string
		err := f.admin.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text),'[]'::jsonb)::text FROM public."+pgx.Identifier{table}.Sanitize()+" s").Scan(&raw)
		planPGOK(t, err, "hash generated fixture rows")
		result[table] = sha256.Sum256([]byte(raw))
	}
	return result
}

func managedOwnerDataGateProbe(t *testing.T, f *migrationPlanFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, _, err := openPlannedMigration(ctx, &f.cfg)
	planPGOK(t, err, "open generated migration pool")
	pool, err := db.DB()
	planPGOK(t, err, "read generated migration pool")
	defer func() { planPGOK(t, pool.Close(), "close generated migration pool") }()
	before := managedOwnerRows(t, f)
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous; _ = writer.Close(); _ = reader.Close() }()
	read := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(io.LimitReader(reader, 1<<20)); read <- raw }()
	backfills, verifiers := 0, 0
	sentinel := errors.New("synthetic verifier boundary")
	err = database.RunRLSMigrationCoordinator(db, &config.DefaultAdminConfig{}, f.runtime,
		func(*gorm.DB) error { backfills++; return errors.New("unexpected synthetic mutation branch") },
		func() error { verifiers++; return sentinel })
	require.NoError(t, writer.Close())
	os.Stdout = previous
	var output string
	select {
	case raw := <-read:
		output = string(raw)
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic observer output did not close")
	}
	require.ErrorIs(t, err, sentinel)
	require.Zero(t, backfills)
	require.Equal(t, 1, verifiers)
	require.Equal(t, before, managedOwnerRows(t, f), "observation must not alter generated rows")
	require.Equal(t, "data-gate empty-phone-legacy-contacts count=0 mode=observe\ndata-gate empty-phone-mixed-sender-contacts count=0 mode=observe\ndata-gate empty-phone-missing-continuation-messages count=0 mode=observe\n", output)
}

func managedOwnerPlannerProbe(t *testing.T, f *migrationPlanFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, state, err := openPlannedMigration(ctx, &f.cfg)
	planPGOK(t, err, "open generated planner pool")
	pool, err := db.DB()
	planPGOK(t, err, "read generated planner pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated planner pool") })
	holdMigrationPlanLock(t, pool)
	before := managedOwnerRows(t, f)
	observation := state.observe(ctx)
	var remaining int
	planPGOK(t, f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=$1 AND application_name IN ($2,$3)", f.database, planRuntimeName, planOwnerName).Scan(&remaining), "verify generated observer disposal")
	require.Zero(t, remaining)
	require.Equal(t, before, managedOwnerRows(t, f), "observation must not alter generated rows")
	require.Empty(t, observation.incomplete)
	require.NotEqual(t, "rereply-plan incomplete", observation.line())
}
