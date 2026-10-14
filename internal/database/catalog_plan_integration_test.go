package database_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
)

var catalogPlanSeedIDs = [10]string{
	"SeedPermissionsAndRoles",
	"SeedSystemRolesForAllOrgs",
	"SeedSystemRolesForAllOrgs/seedSystemRolesForOrg",
	"SeedSystemRolesForAllOrgs/FixSystemRolePermissions",
	"SeedSystemRolesForAllOrgs/ApplyManagerSettingsPolicyMigration",
	"SeedSystemRolesForAllOrgs/MigrateExistingUserRoles",
	"SeedSystemRolesForAllOrgs/legacy-super-admin-update",
	"EnsurePlatformReseller",
	"EnsureReReplyProductCatalog",
	"SeedDefaultWidgets",
}

// These tests borrow the existing disposable catalog fixtures. They never
// adopt the database named by TEST_DATABASE_URL or call a seed to erase drift.
// The command's held-lock and one-shot lifetime tests live in package main.
func catalogPlanTransaction(t *testing.T, ctx context.Context, finalAt time.Time, dsn string) (pgx.Tx, func()) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.RuntimeParams["options"] != "" {
		t.Fatal("synthetic planner configuration refused")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "3000ms"
	cfg.RuntimeParams["lock_timeout"] = "1000ms"
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "5000ms"
	cfg.RuntimeParams["application_name"] = "rereply-plan-integration-test"
	cfg.ConnectTimeout = 3 * time.Second
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("synthetic planner connection failed")
	}
	var tx pgx.Tx
	closed := false
	dispose := func() {
		if closed {
			return
		}
		closed = true
		until := time.Now().Add(2 * time.Second)
		if finalAt.Before(until) {
			until = finalAt
		}
		cleanup, cancel := context.WithDeadline(context.Background(), until)
		defer cancel()
		if tx != nil {
			if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error("synthetic planner transaction cleanup failed")
			}
		}
		if err := conn.Close(cleanup); err != nil {
			t.Error("synthetic planner connection cleanup failed")
		}
		select {
		case <-conn.PgConn().CleanupDone():
		case <-cleanup.Done():
			t.Error("synthetic planner physical cleanup did not finish")
		}
	}
	t.Cleanup(dispose)
	tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		dispose()
		t.Fatal("synthetic planner transaction failed")
	}
	var readOnly, isolation, searchPath string
	var major int
	err = tx.QueryRow(ctx, "SELECT current_setting('transaction_read_only'), current_setting('transaction_isolation'), current_setting('search_path'), current_setting('server_version_num')::integer/10000").Scan(&readOnly, &isolation, &searchPath, &major)
	require.NoError(t, err)
	require.Equal(t, "on", readOnly)
	require.Equal(t, "repeatable read", isolation)
	require.Equal(t, "pg_catalog", searchPath)
	require.Equal(t, 17, major, "planner integration requires actual PostgreSQL 17")
	return tx, dispose
}

func catalogPlanBorrowStillOpen(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	var one int
	require.NoError(t, tx.QueryRow(ctx, "SELECT 1").Scan(&one), "observer must leave its borrowed transaction to the caller")
	require.Equal(t, 1, one)
}

func catalogPlanMapSeeds(t *testing.T, got database.PlanSeedObservations) [10]dbcatalog.PlanSeedObservation {
	t.Helper()
	var mapped [10]dbcatalog.PlanSeedObservation
	for i, observation := range got.Steps {
		require.Equal(t, catalogPlanSeedIDs[i], observation.ID)
		require.Contains(t, []database.PlanSeedState{1, 2, 3}, observation.State)
		mapped[i] = dbcatalog.PlanSeedObservation{
			ID: observation.ID, State: dbcatalog.SeedState(observation.State), Code: observation.Code,
		}
	}
	return mapped
}

func catalogPlanPipeline(t *testing.T, f catalogFixture, source dbcatalog.PlanSource) (dbcatalog.PlanResult, dbcatalog.Snapshot, database.PlanSeedObservations) {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
	defer cancel()
	finalAt := start.Add(17 * time.Second)
	runtimeTx, runtimeDone := catalogPlanTransaction(t, ctx, finalAt, f.runtimeDSN)
	snapshot, err := dbcatalog.CapturePlanInTx(ctx, runtimeTx, source.CaptureCatalog())
	require.NoError(t, err)
	catalogPlanBorrowStillOpen(t, ctx, runtimeTx)
	runtimeDone()
	if t.Failed() {
		t.FailNow()
	}
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	ownerTx, ownerDone := catalogPlanTransaction(t, ctx, finalAt, ownerURL.String())
	observations, err := database.ObservePlanSeeds(ctx, ownerTx)
	require.NoError(t, err)
	catalogPlanBorrowStillOpen(t, ctx, ownerTx)
	ownerDone()
	if t.Failed() {
		t.FailNow()
	}
	result, err := dbcatalog.Plan(ctx, dbcatalog.PlanInput{
		Source: source, Snapshot: snapshot, Seeds: catalogPlanMapSeeds(t, observations),
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Err())
	require.True(t, time.Now().Before(finalAt))
	return result, snapshot, observations
}

// Hash complete persisted rows, including timestamps, soft deletes and link
// identities. Raw synthetic records and credential-bearing fields stay in
// memory and are never included in assertions or saved test output.
func catalogPlanRows(t *testing.T, f catalogFixture) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	for _, table := range []string{
		"permissions", "custom_roles", "role_permissions", "organizations", "users",
		"user_organizations", "resellers", "reseller_members", "plans", "plan_prices",
		"plan_entitlements", "widgets",
	} {
		var rows string
		query := "SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text), '[]'::jsonb)::text FROM public." + pgx.Identifier{table}.Sanitize() + " AS s"
		if f.owner.Raw(query).Scan(&rows).Error != nil {
			t.Fatal("synthetic planner row fingerprint failed")
		}
		result[table] = sha256.Sum256([]byte(rows))
	}
	return result
}

// This independent oracle capture is a separate test operation. It does not
// extend or reset the command's work allowance or claim to test that lifetime.
func catalogPlanAfterSnapshot(t *testing.T, f catalogFixture, source dbcatalog.PlanSource) dbcatalog.Snapshot {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
	defer cancel()
	tx, done := catalogPlanTransaction(t, ctx, start.Add(17*time.Second), f.runtimeDSN)
	snapshot, err := dbcatalog.CapturePlanInTx(ctx, tx, source.CaptureCatalog())
	require.NoError(t, err)
	catalogPlanBorrowStillOpen(t, ctx, tx)
	done()
	require.NoError(t, ctx.Err())
	return snapshot
}

func TestTenantRLS_CatalogPlanReadOnlyBaselines(t *testing.T) {
	for _, profile := range []string{"golden", "production"} {
		t.Run(profile, func(t *testing.T) {
			var f catalogFixture
			var source dbcatalog.PlanSource
			var err error
			if profile == "production" {
				f = catalogEmptyTarget(t)
				require.NoError(t, BootstrapProductionShapeEmptyForTest(f.owner, f.runtime, catalogTemplate.runtime))
				source, err = dbcatalog.BuiltinProductionPlanInputs()
			} else {
				f = catalogClone(t)
				source, err = dbcatalog.BuiltinGoldenPlanInputs()
			}
			require.NoError(t, err)
			before := catalogPlanRows(t, f)
			result, snapshot, observations := catalogPlanPipeline(t, f, source)
			for _, observation := range observations.Steps {
				require.Equal(t, database.PlanSeedState(1), observation.State, observation.ID)
				require.Empty(t, observation.Code, observation.ID)
			}
			require.Equal(t, 0, result.CatalogVersion)
			require.Equal(t, snapshot.SHA256, result.LiveSHA256)
			require.Empty(t, result.KnownDrift)
			require.Empty(t, result.UnknownExcess)
			require.Zero(t, result.OtherSchemaExcess)
			require.Empty(t, result.SeedDriftIDs)
			require.Empty(t, result.Incomplete)
			require.Empty(t, result.PendingIDs)
			require.Empty(t, snapshot.Seeds, "the planner does not substitute the old permissions digest for its ten observations")
			require.Equal(t, before, catalogPlanRows(t, f))
			require.Equal(t, snapshot, catalogPlanAfterSnapshot(t, f, source))
		})
	}
}

func TestTenantRLS_CatalogPlanSeedDriftIsObservedWithoutRepair(t *testing.T) {
	cases := []struct {
		name, id, setup string
		parent          bool
		extraColumn     bool
	}{
		{"permission", catalogPlanSeedIDs[0],
			"UPDATE public.permissions SET resource='synthetic.pr18' WHERE id=(SELECT id FROM public.permissions WHERE deleted_at IS NULL ORDER BY resource,action LIMIT 1)", false, false},
		{"system_roles", catalogPlanSeedIDs[2],
			"UPDATE public.custom_roles SET is_system=false WHERE deleted_at IS NULL AND is_system", true, false},
		{"permission_links", catalogPlanSeedIDs[3],
			"DELETE FROM public.role_permissions WHERE custom_role_id=(SELECT id FROM public.custom_roles WHERE deleted_at IS NULL AND is_system AND name='admin' ORDER BY id LIMIT 1)", true, false},
		{"manager_marker", catalogPlanSeedIDs[4],
			"UPDATE public.organizations SET settings=settings-'system_manager_settings_policy_version' WHERE deleted_at IS NULL", true, false},
		{"legacy_role", catalogPlanSeedIDs[5],
			"UPDATE public.users SET role_id=NULL,role='admin' WHERE email='catalog@example.invalid' AND deleted_at IS NULL", true, true},
		{"reseller", catalogPlanSeedIDs[7],
			"UPDATE public.organizations SET reseller_id=NULL WHERE deleted_at IS NULL", false, false},
		{"product_marker", catalogPlanSeedIDs[8],
			"UPDATE public.plans SET metadata=metadata || '{\"rereply_catalog_seed_version\":0}'::jsonb WHERE scope_key='platform' AND code='rereply-starter' AND deleted_at IS NULL", false, false},
		{"widgets", catalogPlanSeedIDs[9],
			"UPDATE public.widgets SET deleted_at=NOW() WHERE deleted_at IS NULL", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := catalogClone(t)
			source, err := dbcatalog.BuiltinGoldenPlanInputs()
			require.NoError(t, err)
			if tc.extraColumn {
				require.NoError(t, f.owner.Exec("ALTER TABLE public.users ADD COLUMN role text").Error)
			}
			setup := f.owner.Exec(tc.setup)
			require.NoError(t, setup.Error)
			require.Positive(t, setup.RowsAffected, "the synthetic setup must actually create drift")
			before := catalogPlanRows(t, f)
			result, snapshot, observations := catalogPlanPipeline(t, f, source)
			require.Contains(t, result.SeedDriftIDs, tc.id)
			for _, observation := range observations.Steps {
				if observation.ID == tc.id {
					require.Equal(t, database.PlanSeedState(2), observation.State)
				}
			}
			if tc.parent {
				require.Contains(t, result.SeedDriftIDs, catalogPlanSeedIDs[1])
			}
			require.Equal(t, database.PlanSeedState(1), observations.Steps[6].State, "retired escalation is inert")
			require.NotContains(t, result.SeedDriftIDs, catalogPlanSeedIDs[6])
			require.Empty(t, result.PendingIDs, "baseline seed drift does not become executable ledger work")
			require.Empty(t, result.Incomplete, "each positive fixture has complete unambiguous evidence")
			if tc.extraColumn {
				require.NotEmpty(t, result.UnknownExcess, "the extra legacy column must also be reported honestly")
			} else {
				require.Empty(t, result.KnownDrift)
				require.Empty(t, result.UnknownExcess)
			}
			require.Equal(t, before, catalogPlanRows(t, f))
			require.Equal(t, snapshot, catalogPlanAfterSnapshot(t, f, source))
		})
	}
}

func TestTenantRLS_CatalogPlanEmptyTargetCannotAppearClean(t *testing.T) {
	f := catalogEmptyTarget(t)
	source, err := dbcatalog.BuiltinGoldenPlanInputs()
	require.NoError(t, err)
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
	defer cancel()
	finalAt := start.Add(17 * time.Second)
	runtimeTx, runtimeDone := catalogPlanTransaction(t, ctx, finalAt, f.runtimeDSN)
	snapshot, err := dbcatalog.CapturePlanInTx(ctx, runtimeTx, source.CaptureCatalog())
	require.NoError(t, err)
	missing := 0
	for _, object := range snapshot.Objects {
		if object.Missing {
			missing++
		}
	}
	require.Positive(t, missing)
	catalogPlanBorrowStillOpen(t, ctx, runtimeTx)
	runtimeDone()
	if t.Failed() {
		t.FailNow()
	}
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	ownerTx, ownerDone := catalogPlanTransaction(t, ctx, finalAt, ownerURL.String())
	observations, observeErr := database.ObservePlanSeeds(ctx, ownerTx)
	ownerDone()
	if t.Failed() {
		t.FailNow()
	}
	if observeErr == nil {
		incomplete := false
		for _, observation := range observations.Steps {
			incomplete = incomplete || observation.State == database.PlanSeedState(3)
		}
		require.True(t, incomplete, "absent required tables cannot fabricate ten clear observations")
		result, planErr := dbcatalog.Plan(ctx, dbcatalog.PlanInput{
			Source: source, Snapshot: snapshot, Seeds: catalogPlanMapSeeds(t, observations),
		})
		require.NoError(t, planErr)
		require.NotEmpty(t, result.Incomplete)
		require.NotEmpty(t, result.KnownDrift)
		require.Empty(t, result.PendingIDs)
	}
	require.Equal(t, snapshot, catalogPlanAfterSnapshot(t, f, source))
	var objects int64
	require.NoError(t, f.owner.Raw("SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'").Scan(&objects).Error)
	require.Zero(t, objects, "observation must leave the empty target empty")
}
