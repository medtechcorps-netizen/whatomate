package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
)

func TestMigrationPlanSingleDeadlineAndCleanupReserve(t *testing.T) {
	start := time.Now()
	d, ok := newPlanDeadline(context.Background(), start)
	require.True(t, ok)
	require.Equal(t, start.Add(15*time.Second), d.workAt)
	require.Equal(t, start.Add(17*time.Second), d.finalAt)
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(7*time.Second))
	defer cancel()
	d, ok = newPlanDeadline(ctx, start)
	require.True(t, ok)
	require.Equal(t, start.Add(5*time.Second), d.workAt)
	require.Equal(t, start.Add(7*time.Second), d.finalAt)
	// Cancellation kills work, while cleanup still has the original absolute cap.
	cancel()
	require.False(t, d.alive(ctx))
	cleanup, done := d.cleanup()
	defer done()
	require.NoError(t, cleanup.Err())
	until, has := cleanup.Deadline()
	require.True(t, has)
	require.False(t, until.After(d.finalAt))
	short, stop := context.WithDeadline(context.Background(), start.Add(time.Second))
	defer stop()
	_, ok = newPlanDeadline(short, start)
	require.False(t, ok)
}

func TestMigrationPlanStartupCopiesAndPositiveTimeouts(t *testing.T) {
	parsed, err := parseMigrationPlanConfig("host=127.0.0.1 user=owner dbname=fixture TimeZone=Asia/Kuala_Lumpur application_name=old")
	require.NoError(t, err)
	require.Equal(t, "Asia/Kuala_Lumpur", parsed.RuntimeParams["timezone"])
	require.Equal(t, "old", parsed.RuntimeParams["application_name"])
	d := planDeadline{time.Now().Add(1800 * time.Millisecond), time.Now().Add(3800 * time.Millisecond)}
	observer, err := planObserverConfig(parsed, planOwnerName, d, context.Background())
	require.NoError(t, err)
	require.Equal(t, "old", parsed.RuntimeParams["application_name"])
	require.Equal(t, "Asia/Kuala_Lumpur", observer.RuntimeParams["timezone"])
	require.Equal(t, "on", observer.RuntimeParams["default_transaction_read_only"])
	require.Equal(t, "repeatable read", observer.RuntimeParams["default_transaction_isolation"])
	require.Equal(t, "pg_catalog", observer.RuntimeParams["search_path"])
	for _, key := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
		value, err := time.ParseDuration(observer.RuntimeParams[key])
		require.NoError(t, err)
		require.GreaterOrEqual(t, value, time.Millisecond)
		require.LessOrEqual(t, value, 1800*time.Millisecond)
	}
	observer.RuntimeParams["timezone"] = "UTC"
	require.Equal(t, "Asia/Kuala_Lumpur", parsed.RuntimeParams["timezone"])
	parsed.RuntimeParams["options"] = "-c search_path=public"
	_, err = planObserverConfig(parsed, planOwnerName, d, context.Background())
	require.Error(t, err)
	delete(parsed.RuntimeParams, "options")
	d.workAt = time.Now().Add(time.Microsecond)
	_, err = planObserverConfig(parsed, planOwnerName, d, context.Background())
	require.Error(t, err, "sub-millisecond time must not turn PostgreSQL timeout zero into unlimited work")
}

func TestMigrationPlanMarkerAndSpentSlot(t *testing.T) {
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	marker := migrationPlanMarker(id)
	require.Len(t, marker, 54)
	require.Equal(t, "rereply-plan-lock-"+id.String(), marker)
	require.Empty(t, migrationPlanMarker(uuid.Nil))
	s := &migrationPlanState{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := s.observe(ctx)
	require.Equal(t, "deadline", first.incomplete[0].Code)
	require.Equal(t, planSpent, s.slot.Load())
	require.Equal(t, "spent", s.observe(context.Background()).incomplete[0].Code)
	s.slot.Store(planDisabled)
	require.Equal(t, "disabled", s.observe(context.Background()).incomplete[0].Code)
	require.Equal(t, planDisabled, s.slot.Load())
}

func TestMigrationPlanHeldWitnessRejectsAmbiguityAndChangedIdentity(t *testing.T) {
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	marker := migrationPlanMarker(id)
	self := planSelfIdentity{pid: 41, databaseOID: 51, database: "fixture", roleOID: 61, role: "runtime"}
	held := planHeldIdentity{PID: 42, DatabaseOID: 51, DatabaseName: "fixture", RoleOID: 62, RoleName: "owner", ApplicationName: marker}
	w := planWitness{ObserverPID: 41, ObserverDatabaseOID: 51, ObserverDatabaseName: "fixture", TokenRows: 1, LockRows: 1, JoinedRows: 1, Held: &held}
	require.True(t, validPlanHeld(w, self, marker, "owner", "fixture", &held))
	for _, mutate := range []func(*planWitness){
		func(v *planWitness) { v.TokenRows = 2 }, func(v *planWitness) { v.TokenRows = 0 },
		func(v *planWitness) { v.LockRows = 0 }, func(v *planWitness) { v.JoinedRows = 0 },
		func(v *planWitness) { v.ObserverDatabaseOID++ }, func(v *planWitness) { v.ObserverPID++ },
		func(v *planWitness) { v.Held = nil },
	} {
		v := w
		mutate(&v)
		require.False(t, validPlanHeld(v, self, marker, "owner", "fixture", &held))
	}
	for _, mutate := range []func(*planHeldIdentity){
		func(v *planHeldIdentity) { v.PID = self.pid }, func(v *planHeldIdentity) { v.DatabaseOID++ },
		func(v *planHeldIdentity) { v.DatabaseName = "foreign" }, func(v *planHeldIdentity) { v.RoleOID++ },
		func(v *planHeldIdentity) { v.RoleName = "runtime" }, func(v *planHeldIdentity) { v.ApplicationName = "stale" },
	} {
		copyHeld := held
		mutate(&copyHeld)
		v := w
		v.Held = &copyHeld
		require.False(t, validPlanHeld(v, self, marker, "owner", "fixture", &held))
	}
	// Equal OIDs on another server are not enough: its nonce has no matching row.
	foreign := w
	foreign.TokenRows = 0
	foreign.JoinedRows = 0
	foreign.Held = nil
	require.False(t, validPlanHeld(foreign, self, marker, "owner", "fixture", &held))
}

func TestMigrationPlanSelfLifetimeAndSeedMapping(t *testing.T) {
	a := planSelfIdentity{pid: 1, databaseOID: 2, roleOID: 3, database: "fixture", role: "owner", backendStart: time.Now()}
	require.True(t, samePlanSelf(a, a))
	b := a
	b.backendStart = b.backendStart.Add(time.Nanosecond)
	require.False(t, samePlanSelf(a, b))
	b = a
	b.bypassRLS = true
	require.False(t, samePlanSelf(a, b))
	var raw database.PlanSeedObservations
	_, ok := planSeedInput(raw)
	require.False(t, ok, "uninitialized observations cannot become clear")
	for i := range raw.Steps {
		raw.Steps[i] = database.PlanSeedObservation{ID: "SeedPermissionsAndRoles", State: database.PlanSeedClear}
	}
	raw.Steps[1].State = database.PlanSeedDrift
	raw.Steps[2].State = database.PlanSeedIncomplete
	raw.Steps[2].Code = "conflict"
	mapped, ok := planSeedInput(raw)
	require.True(t, ok)
	require.Equal(t, dbcatalog.SeedClear, mapped[0].State)
	require.Equal(t, dbcatalog.SeedDrift, mapped[1].State)
	require.Equal(t, dbcatalog.SeedIncomplete, mapped[2].State)
	require.Equal(t, "conflict", mapped[2].Code)
	// Exact IDs/order/parent semantics are subsequently validated by Plan itself.
}

func TestMigrationPlanOutputNeverTreatsIncompleteAsZero(t *testing.T) {
	d, ok := newPlanDeadline(context.Background(), time.Now())
	require.True(t, ok)
	o := planObservation{parent: context.Background(), deadline: d, result: dbcatalog.PlanResult{LiveSHA256: strings.Repeat("a", 64), SeedDriftIDs: []string{"SeedDefaultWidgets"}}}
	line := o.line()
	require.Equal(t, "rereply-plan catalog_version=0 live_sha256=aaaaaaaaaaaa pending=0 drift=0 unknown=0 seed_drift_steps=SeedDefaultWidgets", line)
	require.Contains(t, line, "live_sha256=aaaaaaaaaaaa")
	require.Contains(t, line, "seed_drift_steps=SeedDefaultWidgets")
	require.NotContains(t, line, strings.Repeat("a", 64))
	for _, mutate := range []func(*planObservation){
		func(v *planObservation) {
			v.incomplete = []dbcatalog.PlanIssue{{Component: "private", Code: "password=secret"}}
		},
		func(v *planObservation) {
			v.result.Incomplete = []dbcatalog.PlanIssue{{Component: "SeedDefaultWidgets", Code: "unavailable"}}
		},
		func(v *planObservation) { v.result.SeedDriftIDs = []string{"private@example.invalid"} },
		func(v *planObservation) { v.result.LiveSHA256 = "private" },
		func(v *planObservation) { v.deadline.workAt = time.Now().Add(-time.Second) },
	} {
		v := o
		mutate(&v)
		require.Equal(t, "rereply-plan incomplete", v.line())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.parent = ctx
	require.Equal(t, "rereply-plan incomplete", o.line())
}

func TestMigrationPlanOutputAggregatesOnlyPositiveExcessWithoutOverflow(t *testing.T) {
	d, ok := newPlanDeadline(context.Background(), time.Now())
	require.True(t, ok)
	o := planObservation{parent: context.Background(), deadline: d, result: dbcatalog.PlanResult{
		LiveSHA256:        strings.Repeat("b", 64),
		KnownDrift:        []dbcatalog.Difference{{Kind: "relations", Identity: "organizations", Status: "changed"}, {Kind: "indexes", Identity: "organizations.example", Status: "missing"}},
		UnknownExcess:     []dbcatalog.UnknownCount{{Kind: "columns", Parent: "organizations", Count: 3}, {Kind: "relations", Count: 5}},
		OtherSchemaExcess: 2,
	}}
	require.Equal(t, "rereply-plan catalog_version=0 live_sha256=bbbbbbbbbbbb pending=0 drift=2 unknown=10 seed_drift_steps=", o.line())
	for _, counts := range []struct {
		schema  int64
		buckets []dbcatalog.UnknownCount
	}{
		{-1, nil},
		{2, []dbcatalog.UnknownCount{{Kind: "relations", Count: -1}}},
		{0, []dbcatalog.UnknownCount{{Kind: "relations", Count: 0}}},
		{1, []dbcatalog.UnknownCount{{Kind: "relations", Count: 1<<63 - 1}}},
		{0, []dbcatalog.UnknownCount{{Kind: "columns", Count: 1<<63 - 1}, {Kind: "relations", Count: 1}}},
	} {
		v := o
		v.result.OtherSchemaExcess = counts.schema
		v.result.UnknownExcess = counts.buckets
		require.Equal(t, "rereply-plan incomplete", v.line())
	}
}

func TestMigrationPlanConfigurationErrorsAreFixedAndDoNotConnect(t *testing.T) {
	_, err := parseMigrationPlanConfig("postgres://private:do-not-print@/%xx")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "do-not-print")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, state, err := openPlannedMigration(ctx, &config.DatabaseConfig{MigrationURL: "postgres://private:do-not-print@127.0.0.1:1/private"})
	require.Error(t, err)
	require.Nil(t, db)
	require.Nil(t, state)
	require.NotContains(t, err.Error(), "do-not-print")
	var c *pgx.ConnConfig
	d, _ := newPlanDeadline(context.Background(), time.Now())
	_, err = planObserverConfig(c, planOwnerName, d, context.Background())
	require.Error(t, err)
}
