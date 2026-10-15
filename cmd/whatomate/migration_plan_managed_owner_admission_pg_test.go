package main

import (
	"context"
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

func TestMigrationPlanPGManagedOwnerCanonicalPolicyGuards(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEDB CREATEROLE BYPASSRLS")
	planPGOK(t, err, "configure generated managed owner")
	for _, table := range []string{"contacts", "widgets"} {
		_, err = f.admin.Exec(ctx, "ALTER POLICY rereply_migration_access ON public."+pgx.Identifier{table}.Sanitize()+" USING (false) WITH CHECK (false)")
		planPGOK(t, err, "tamper generated migration policy")
	}
	before := managedOwnerRows(t, f)
	db, state, err := openPlannedMigration(ctx, &f.cfg)
	planPGOK(t, err, "open generated migration pool")
	pool, err := db.DB()
	planPGOK(t, err, "read generated migration pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated migration pool") })
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous; _ = writer.Close(); _ = reader.Close() }()
	read := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(io.LimitReader(reader, 1<<20)); read <- raw }()
	backfills := 0
	err = database.RunRLSMigrationCoordinator(db, &config.DefaultAdminConfig{}, f.runtime,
		func(*gorm.DB) error { backfills++; return errors.New("unexpected synthetic mutation branch") },
		func() error { return errors.New("synthetic verifier boundary") })
	require.NoError(t, writer.Close())
	os.Stdout = previous
	var output string
	select {
	case raw := <-read:
		output = string(raw)
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic observer output did not close")
	}
	require.Error(t, err)
	require.Zero(t, backfills)
	require.Equal(t, "data-gate empty-phone-legacy-contacts error\ndata-gate empty-phone-mixed-sender-contacts error\ndata-gate empty-phone-missing-continuation-messages error\n", output)
	holdMigrationPlanLock(t, pool)
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	work, stop := context.WithDeadline(ctx, d.workAt)
	defer stop()
	cfg, err := planObserverConfig(state.owner, planOwnerName, d, work)
	planPGOK(t, err, "configure owner observer")
	conn, tx, err := openPlanSession(work, cfg)
	planPGOK(t, err, "open owner observer")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	self, err := readPlanSelf(work, tx, planOwnerName, f.owner, false)
	planPGOK(t, err, "read owner observer")
	held, err := readPlanHeld(work, tx, self, state.marker, f.owner, f.database, nil)
	planPGOK(t, err, "read real held owner")
	seeds, err := database.ObservePlanSeedsOnHeldOwner(work, tx, database.PlanSeedHeldOwner{
		PID: held.PID, DatabaseOID: held.DatabaseOID, DatabaseName: held.DatabaseName,
		RoleOID: held.RoleOID, RoleName: held.RoleName, Marker: held.ApplicationName,
	})
	planPGOK(t, err, "recover from unavailable canonical policy")
	require.Equal(t, "SeedDefaultWidgets", seeds.Steps[9].ID)
	require.Equal(t, database.PlanSeedIncomplete, seeds.Steps[9].State)
	require.Equal(t, "unavailable", seeds.Steps[9].Code)
	require.Equal(t, before, managedOwnerRows(t, f))
}

func TestMigrationPlanPGManagedOwnerAdmissionBoundaries(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEDB CREATEROLE BYPASSRLS")
	planPGOK(t, err, "configure generated managed owner")
	db, state, err := openPlannedMigration(ctx, &f.cfg)
	planPGOK(t, err, "open generated migration pool")
	pool, err := db.DB()
	planPGOK(t, err, "read generated migration pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated migration pool") })
	heldConn := holdMigrationPlanLock(t, pool)
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	work, stop := context.WithDeadline(ctx, d.workAt)
	defer stop()
	cfg, err := planObserverConfig(state.owner, planOwnerName, d, work)
	planPGOK(t, err, "configure owner observer")
	conn, tx, err := openPlanSession(work, cfg)
	planPGOK(t, err, "open owner observer")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	self, err := readPlanSelf(work, tx, planOwnerName, f.owner, false)
	planPGOK(t, err, "read owner observer")
	held, err := readPlanHeld(work, tx, self, state.marker, f.owner, f.database, nil)
	planPGOK(t, err, "read real held owner")
	owner := database.PlanSeedHeldOwner{PID: held.PID, DatabaseOID: held.DatabaseOID, DatabaseName: held.DatabaseName,
		RoleOID: held.RoleOID, RoleName: held.RoleName, Marker: held.ApplicationName}
	_, err = database.ObservePlanSeeds(work, tx)
	require.EqualError(t, err, "seed observation session unavailable", "generic API must stay strict")
	wrong := []database.PlanSeedHeldOwner{owner, owner, owner, owner, owner, owner, owner}
	wrong[0].PID++
	wrong[1].DatabaseOID++
	wrong[2].RoleOID++
	wrong[3].DatabaseName += "_other"
	wrong[4].RoleName += "_other"
	wrong[5].Marker = "rereply-plan-lock-00000000-0000-0000-0000-000000000000"
	wrong[6].PID = self.pid
	for i, candidate := range wrong {
		_, err = database.ObservePlanSeedsOnHeldOwner(work, tx, candidate)
		require.EqualError(t, err, "seed observation session unavailable", "mismatched context %d", i)
		var one int
		planPGOK(t, tx.QueryRow(work, "SELECT 1").Scan(&one), "retain borrowed read-only transaction")
		require.Equal(t, 1, one)
	}
	duplicate := state.owner.Copy()
	duplicate.RuntimeParams["application_name"] = state.marker
	other, err := pgx.ConnectConfig(work, duplicate)
	planPGOK(t, err, "open generated duplicate marker")
	_, duplicateErr := database.ObservePlanSeedsOnHeldOwner(work, tx, owner)
	planPGOK(t, other.Close(work), "close generated duplicate marker")
	select {
	case <-other.PgConn().CleanupDone():
	case <-work.Done():
		t.Fatal("generated duplicate marker cleanup incomplete")
	}
	require.EqualError(t, duplicateErr, "seed observation session unavailable")
	before := managedOwnerRows(t, f)
	seeds, err := database.ObservePlanSeedsOnHeldOwner(work, tx, owner)
	planPGOK(t, err, "observe explicit managed owner")
	for _, seed := range seeds.Steps {
		require.Equal(t, database.PlanSeedClear, seed.State)
	}
	require.Equal(t, before, managedOwnerRows(t, f))
	var unlocked bool
	planPGOK(t, heldConn.QueryRowContext(work, "SELECT pg_advisory_unlock(hashtext(current_database()),1380270905)").Scan(&unlocked), "release generated witness")
	require.True(t, unlocked)
	// Restore before asserting so the existing fixture cleanup always owns its
	// expected lock. The observation itself occurs while the lock is absent.
	_, observeErr := database.ObservePlanSeedsOnHeldOwner(work, tx, owner)
	var locked bool
	planPGOK(t, heldConn.QueryRowContext(work, "SELECT pg_try_advisory_lock(hashtext(current_database()),1380270905)").Scan(&locked), "restore generated witness")
	require.True(t, locked)
	require.EqualError(t, observeErr, "seed observation session unavailable")
}

func TestMigrationPlanPGManagedOwnerRequiresDatabaseOwnership(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	for _, scenario := range []struct {
		name    string
		managed bool
		ownsDB  bool
	}{
		{name: "restricted_schema_owner", managed: false, ownsDB: false},
		{name: "privileged_schema_owner", managed: true, ownsDB: false},
		{name: "privileged_database_owner", managed: true, ownsDB: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			attributes := "NOCREATEROLE NOBYPASSRLS"
			if scenario.managed {
				attributes = "CREATEROLE BYPASSRLS"
			}
			_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" "+attributes)
			planPGOK(t, err, "configure generated owner boundary")
			databaseOwner := f.admin.Config().User
			if scenario.ownsDB {
				databaseOwner = f.owner
			}
			_, err = f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.database}.Sanitize()+" OWNER TO "+pgx.Identifier{databaseOwner}.Sanitize())
			planPGOK(t, err, "configure generated database owner boundary")
			if scenario.managed && !scenario.ownsDB {
				managedOwnerDataGateRefusalProbe(t, f)
			} else {
				managedOwnerDataGateProbe(t, f)
			}
			db, state, err := openPlannedMigration(ctx, &f.cfg)
			planPGOK(t, err, "open generated migration pool")
			pool, err := db.DB()
			planPGOK(t, err, "read generated migration pool")
			t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated migration pool") })
			holdMigrationPlanLock(t, pool)
			d, ok := newPlanDeadline(ctx, time.Now())
			require.True(t, ok)
			work, stop := context.WithDeadline(ctx, d.workAt)
			defer stop()
			cfg, err := planObserverConfig(state.owner, planOwnerName, d, work)
			planPGOK(t, err, "configure owner observer")
			conn, tx, err := openPlanSession(work, cfg)
			planPGOK(t, err, "open owner observer")
			defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
			self, err := readPlanSelf(work, tx, planOwnerName, f.owner, false)
			planPGOK(t, err, "read owner observer")
			held, err := readPlanHeld(work, tx, self, state.marker, f.owner, f.database, nil)
			planPGOK(t, err, "read real held owner")
			owner := database.PlanSeedHeldOwner{PID: held.PID, DatabaseOID: held.DatabaseOID, DatabaseName: held.DatabaseName,
				RoleOID: held.RoleOID, RoleName: held.RoleName, Marker: held.ApplicationName}
			before := managedOwnerRows(t, f)
			_, genericErr := database.ObservePlanSeeds(work, tx)
			_, heldErr := database.ObservePlanSeedsOnHeldOwner(work, tx, owner)
			if scenario.managed {
				require.EqualError(t, genericErr, "seed observation session unavailable")
			} else {
				planPGOK(t, genericErr, "retain restricted schema-owner generic admission")
			}
			if scenario.managed && !scenario.ownsDB {
				require.EqualError(t, heldErr, "seed observation session unavailable")
			} else {
				planPGOK(t, heldErr, "observe explicitly held eligible owner")
			}
			require.Equal(t, before, managedOwnerRows(t, f))
		})
	}
}

func managedOwnerDataGateRefusalProbe(t *testing.T, f *migrationPlanFixture) {
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
	require.Equal(t, before, managedOwnerRows(t, f))
	require.Equal(t, "data-gate empty-phone-legacy-contacts error\ndata-gate empty-phone-mixed-sender-contacts error\ndata-gate empty-phone-missing-continuation-messages error\n", output)
}

func TestMigrationPlanPGManagedOwnerDoesNotAdmitPrivilegedRuntime(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEDB CREATEROLE BYPASSRLS")
	planPGOK(t, err, "configure generated managed owner")
	for _, privilege := range []string{"CREATEROLE", "BYPASSRLS", "REPLICATION"} {
		t.Run(privilege, func(t *testing.T) {
			attributes := map[string]string{
				"CREATEROLE":  "CREATEROLE NOBYPASSRLS NOREPLICATION",
				"BYPASSRLS":   "NOCREATEROLE BYPASSRLS NOREPLICATION",
				"REPLICATION": "NOCREATEROLE NOBYPASSRLS REPLICATION",
			}[privilege]
			_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.runtime}.Sanitize()+" "+attributes)
			planPGOK(t, err, "configure generated runtime negative")
			db, state, err := openPlannedMigration(ctx, &f.cfg)
			planPGOK(t, err, "open generated migration pool")
			pool, err := db.DB()
			planPGOK(t, err, "read generated migration pool")
			t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated migration pool") })
			holdMigrationPlanLock(t, pool)
			observation := state.observe(ctx)
			require.Len(t, observation.incomplete, 1)
			require.Equal(t, "runtime", observation.incomplete[0].Component)
			require.Equal(t, "identity", observation.incomplete[0].Code)
			require.Equal(t, "rereply-plan incomplete", observation.line())
		})
	}
}
