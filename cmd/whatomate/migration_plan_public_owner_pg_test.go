package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
)

// All DDL below is limited to the existing generated, loopback-only PG17
// fixture. No role/database named by TEST_DATABASE_URL is altered or adopted.
func TestMigrationPlanPGPublicDatabaseOwnerRegression(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEDB CREATEROLE BYPASSRLS REPLICATION")
	planPGOK(t, err, "configure generated managed owner")
	source, err := dbcatalog.BuiltinProductionPlanInputs()
	planPGOK(t, err, "load fixed plan source")
	globals, err := dbcatalog.BuiltinGlobalTables()
	planPGOK(t, err, "load fixed catalog globals")
	for _, scenario := range []struct {
		name, schemaOwner string
		direct            bool
	}{{"direct_owner_control", f.owner, true}, {"builtin_database_owner", "pg_database_owner", false}} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO "+pgx.Identifier{scenario.schemaOwner}.Sanitize())
			planPGOK(t, err, "set generated schema owner")
			before := managedOwnerRows(t, f)
			raw, err := dbcatalog.Capture(ctx, f.cfg.URL, dbcatalog.Options{Catalog: source.CaptureCatalog(), GlobalTables: globals, Timeout: 15 * time.Second})
			planPGOK(t, err, "capture unchanged public catalog semantics")
			require.True(t, raw.Ownership.MigrationOwnerOwnsDatabase)
			require.Equal(t, scenario.direct, raw.Ownership.MigrationOwnerOwnsPublicSchema)
			// A pure Plan receives only its input facts. It must not acquire the
			// new effective-ownership interpretation from shared catalog capture.
			ids := [...]string{"SeedPermissionsAndRoles", "SeedSystemRolesForAllOrgs",
				"SeedSystemRolesForAllOrgs/seedSystemRolesForOrg", "SeedSystemRolesForAllOrgs/FixSystemRolePermissions",
				"SeedSystemRolesForAllOrgs/ApplyManagerSettingsPolicyMigration", "SeedSystemRolesForAllOrgs/MigrateExistingUserRoles",
				"SeedSystemRolesForAllOrgs/legacy-super-admin-update", "EnsurePlatformReseller", "EnsureReReplyProductCatalog", "SeedDefaultWidgets"}
			var seeds [10]dbcatalog.PlanSeedObservation
			for i, id := range ids {
				seeds[i] = dbcatalog.PlanSeedObservation{ID: id, State: dbcatalog.SeedClear}
			}
			plan, err := dbcatalog.Plan(ctx, dbcatalog.PlanInput{Source: source, Snapshot: raw, Seeds: seeds})
			planPGOK(t, err, "compare unchanged pure plan ownership")
			if scenario.direct {
				require.Empty(t, plan.Incomplete)
			} else {
				require.Equal(t, []dbcatalog.PlanIssue{{Component: "catalog", Code: "ownership"}}, plan.Incomplete)
			}
			// This invokes the real coordinator witness, restricted runtime,
			// separate native owner, actual seed observations and bounded cleanup.
			managedOwnerPlannerProbe(t, f)
			after, err := dbcatalog.Capture(ctx, f.cfg.URL, dbcatalog.Options{Catalog: source.CaptureCatalog(), GlobalTables: globals, Timeout: 15 * time.Second})
			planPGOK(t, err, "recapture unchanged public catalog")
			require.Equal(t, raw, after)
			require.Equal(t, before, managedOwnerRows(t, f))
		})
	}
}

func withPublicOwnerRuntime(t *testing.T, f *migrationPlanFixture, inspect func(context.Context, pgx.Tx, planHeldIdentity)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	db, state, err := openPlannedMigration(ctx, &f.cfg)
	planPGOK(t, err, "open generated public-owner planner")
	pool, err := db.DB()
	planPGOK(t, err, "get generated public-owner pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated public-owner pool") })
	holdMigrationPlanLock(t, pool)
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	work, stop := context.WithDeadline(ctx, d.workAt)
	defer stop()
	parsed, err := pgx.ParseConfig(f.cfg.URL)
	planPGOK(t, err, "parse generated runtime")
	cfg, err := planObserverConfig(parsed, planRuntimeName, d, work)
	planPGOK(t, err, "configure generated runtime")
	conn, tx, err := openPlanSession(work, cfg)
	planPGOK(t, err, "open generated runtime")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	self, err := readPlanSelf(work, tx, planRuntimeName, f.runtime, true)
	planPGOK(t, err, "verify generated runtime restriction")
	held, err := readPlanHeld(work, tx, self, state.marker, f.owner, f.database, nil)
	planPGOK(t, err, "verify generated held owner")
	inspect(work, tx, held)
}

func TestMigrationPlanPGPublicDatabaseOwnerBoundaries(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO pg_database_owner")
	planPGOK(t, err, "set generated builtin schema owner")
	for _, name := range []string{"database_oid", "database_name", "owner_oid", "owner_name"} {
		t.Run(name, func(t *testing.T) {
			withPublicOwnerRuntime(t, f, func(ctx context.Context, tx pgx.Tx, held planHeldIdentity) {
				switch name {
				case "database_oid":
					held.DatabaseOID++
				case "database_name":
					held.DatabaseName += "_other"
				case "owner_oid":
					held.RoleOID++
				case "owner_name":
					held.RoleName += "_other"
				}
				value := dbcatalog.Snapshot{Ownership: dbcatalog.Ownership{MigrationOwnerOwnsDatabase: true}}
				require.EqualError(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "plan schema ownership unavailable")
				require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
			})
		})
	}
	t.Run("different_database_owner", func(t *testing.T) {
		_, err := f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.database}.Sanitize()+" OWNER TO "+pgx.Identifier{f.admin.Config().User}.Sanitize())
		planPGOK(t, err, "change generated database owner")
		t.Cleanup(func() {
			_, err := f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.database}.Sanitize()+" OWNER TO "+pgx.Identifier{f.owner}.Sanitize())
			planPGOK(t, err, "restore generated database owner")
		})
		withPublicOwnerRuntime(t, f, func(ctx context.Context, tx pgx.Tx, held planHeldIdentity) {
			value := dbcatalog.Snapshot{Ownership: dbcatalog.Ownership{MigrationOwnerOwnsDatabase: true}}
			require.EqualError(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "plan schema ownership unavailable")
			require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
		})
	})
	t.Run("different_organizations_owner", func(t *testing.T) {
		_, err := f.admin.Exec(ctx, "ALTER TABLE public.organizations OWNER TO "+pgx.Identifier{f.admin.Config().User}.Sanitize())
		planPGOK(t, err, "change generated anchor owner")
		t.Cleanup(func() {
			_, err := f.admin.Exec(ctx, "ALTER TABLE public.organizations OWNER TO "+pgx.Identifier{f.owner}.Sanitize())
			planPGOK(t, err, "restore generated anchor owner")
		})
		withPublicOwnerRuntime(t, f, func(ctx context.Context, tx pgx.Tx, held planHeldIdentity) {
			value := dbcatalog.Snapshot{Ownership: dbcatalog.Ownership{MigrationOwnerOwnsDatabase: true}}
			require.EqualError(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "plan schema ownership unavailable")
			require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
		})
	})
	t.Run("arbitrary_schema_owner_membership", func(t *testing.T) {
		other := f.owner + "_schema"
		_, err := f.admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{other}.Sanitize()+" NOLOGIN NOSUPERUSER")
		planPGOK(t, err, "create generated alternate schema role")
		t.Cleanup(func() {
			_, err := f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO pg_database_owner")
			planPGOK(t, err, "restore generated builtin schema owner")
			_, err = f.admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{other}.Sanitize())
			planPGOK(t, err, "drop generated alternate schema role")
		})
		_, err = f.admin.Exec(ctx, "GRANT "+pgx.Identifier{other}.Sanitize()+" TO "+pgx.Identifier{f.owner}.Sanitize()+" WITH INHERIT TRUE")
		planPGOK(t, err, "grant generated alternate role membership")
		_, err = f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO "+pgx.Identifier{other}.Sanitize())
		planPGOK(t, err, "set generated alternate schema owner")
		var inherited bool
		planPGOK(t, f.admin.QueryRow(ctx, "SELECT pg_catalog.pg_has_role($1,$2,'USAGE')", f.owner, other).Scan(&inherited), "verify generated effective membership")
		require.True(t, inherited, "negative must have actual effective membership")
		withPublicOwnerRuntime(t, f, func(ctx context.Context, tx pgx.Tx, held planHeldIdentity) {
			value := dbcatalog.Snapshot{Ownership: dbcatalog.Ownership{MigrationOwnerOwnsDatabase: true}}
			require.EqualError(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "plan schema ownership unavailable")
			require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
		})
	})
}

func TestMigrationPlanPGPublicDatabaseOwnerFailureGuards(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO pg_database_owner")
	planPGOK(t, err, "set generated builtin schema owner")
	t.Run("cancellation_and_query_failure", func(t *testing.T) {
		withPublicOwnerRuntime(t, f, func(ctx context.Context, tx pgx.Tx, held planHeldIdentity) {
			value := dbcatalog.Snapshot{Ownership: dbcatalog.Ownership{MigrationOwnerOwnsDatabase: true}}
			cancelled, stop := context.WithCancel(ctx)
			stop()
			expired, stopExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer stopExpired()
			for _, invalid := range []context.Context{nil, context.Background(), cancelled, expired} {
				require.EqualError(t, observePlanPublicSchemaOwnership(invalid, tx, &value, held), "plan schema ownership unavailable")
				require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
			}
			_, err := tx.Exec(ctx, "SAVEPOINT public_owner_query_failure")
			planPGOK(t, err, "save generated query boundary")
			_, err = tx.Exec(ctx, "SELECT 1/0")
			require.Error(t, err)
			require.EqualError(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "plan schema ownership unavailable")
			require.False(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
			_, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT public_owner_query_failure")
			planPGOK(t, err, "recover generated query boundary")
			_, err = tx.Exec(ctx, "RELEASE SAVEPOINT public_owner_query_failure")
			planPGOK(t, err, "release generated query boundary")
			planPGOK(t, observePlanPublicSchemaOwnership(ctx, tx, &value, held), "observe builtin schema owner after recovery")
			require.True(t, value.Ownership.MigrationOwnerOwnsPublicSchema)
			var readOnly, isolation, searchPath string
			planPGOK(t, tx.QueryRow(ctx, "SELECT current_setting('transaction_read_only'),current_setting('transaction_isolation'),current_setting('search_path')").Scan(&readOnly, &isolation, &searchPath), "verify borrowed session unchanged")
			require.Equal(t, "on", readOnly)
			require.Equal(t, "repeatable read", isolation)
			require.Equal(t, "pg_catalog", searchPath)
		})
	})
	t.Run("held_lock_still_required", func(t *testing.T) {
		db, state, err := openPlannedMigration(ctx, &f.cfg)
		planPGOK(t, err, "open generated witness planner")
		pool, err := db.DB()
		planPGOK(t, err, "get generated witness pool")
		t.Cleanup(func() { planPGOK(t, pool.Close(), "close generated witness pool") })
		held := holdMigrationPlanLock(t, pool)
		var unlocked, locked bool
		planPGOK(t, held.QueryRowContext(ctx, "SELECT pg_advisory_unlock(hashtext(current_database()),1380270905)").Scan(&unlocked), "remove generated held witness")
		require.True(t, unlocked)
		observation := state.observe(ctx)
		planPGOK(t, held.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext(current_database()),1380270905)").Scan(&locked), "restore generated held witness")
		require.True(t, locked)
		require.Equal(t, "rereply-plan incomplete", observation.line())
		require.Equal(t, []dbcatalog.PlanIssue{{Component: "runtime", Code: "target"}}, observation.incomplete)
	})
}
