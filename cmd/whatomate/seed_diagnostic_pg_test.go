package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

// All writes below belong to the existing generated, loopback-only PG17 test
// fixture. The diagnostic itself never creates, seeds or changes these rows.
func newSeedDiagnosticFixture(t *testing.T) (*migrationPlanFixture, config.DatabaseConfig) {
	t.Helper()
	f := newMigrationPlanFixture(t, false)
	cfg := f.cfg
	cfg.URL = cfg.MigrationURL
	cfg.MigrationURL = "must-not-be-used"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, cfg.URL)
	planPGOK(t, err, "connect generated diagnostic owner")
	defer func() { planPGOK(t, conn.Close(ctx), "close generated diagnostic owner") }()
	_, err = conn.Exec(ctx, `CREATE TABLE public.permissions(resource text,action text,deleted_at timestamptz);
CREATE TABLE public.resellers(id uuid PRIMARY KEY,slug text,status text,deleted_at timestamptz);
CREATE TABLE public.organizations(id uuid PRIMARY KEY,reseller_id uuid,deleted_at timestamptz);
CREATE TABLE public.users(id uuid PRIMARY KEY,is_super_admin boolean,is_active boolean,deleted_at timestamptz);
CREATE TABLE public.reseller_members(reseller_id uuid,user_id uuid,role text,is_active boolean,deleted_at timestamptz);`)
	planPGOK(t, err, "create fixed diagnostic relations")
	for _, p := range models.DefaultPermissions() {
		_, err = conn.Exec(ctx, "INSERT INTO public.permissions(resource,action) VALUES($1,$2)", p.Resource, p.Action)
		planPGOK(t, err, "populate generated compiled permission")
	}
	_, err = conn.Exec(ctx, "INSERT INTO public.resellers(id,slug,status) VALUES('00000000-0000-4000-8000-000000000001',$1,$2)", database.PlatformResellerSlug, models.ResellerStatusActive)
	planPGOK(t, err, "populate generated platform")
	return f, cfg
}

func readSeedDiagnosticFixture(t *testing.T, cfg config.DatabaseConfig) database.SeedDiagnosticReport {
	t.Helper()
	raw, err := collectSeedDiagnostic(context.Background(), cfg)
	planPGOK(t, err, "observe generated seed diagnostic")
	require.LessOrEqual(t, len(raw), 64*1024+1)
	var result struct {
		database.SeedDiagnosticReport
		SessionBeforeAfterMatch bool `json:"session_before_after_match"`
		SessionDisposed         bool `json:"session_disposed"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.True(t, result.SessionBeforeAfterMatch)
	require.True(t, result.SessionDisposed)
	require.True(t, result.CompleteVisibility)
	require.True(t, result.DiagnosticOnly)
	require.False(t, result.Writes)
	require.Equal(t, 1, result.Version)
	require.Equal(t, "seed_diagnostic", result.Purpose)
	require.NotContains(t, string(raw), "private-fixture-key")
	require.NotContains(t, string(raw), "00000000-0000-4000-8000-")
	return result.SeedDiagnosticReport
}

func seedDiagnosticFixtureRows(t *testing.T, f *migrationPlanFixture) map[string][32]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(map[string][32]byte)
	for _, table := range []string{"permissions", "resellers", "reseller_members", "organizations", "users"} {
		var raw string
		err := f.admin.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM public."+pgx.Identifier{table}.Sanitize()+" t").Scan(&raw)
		planPGOK(t, err, "fingerprint generated diagnostic rows")
		result[table] = sha256.Sum256([]byte(raw))
	}
	return result
}

func TestSeedDiagnosticPGPermissionDiffAndNoWrites(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	before := seedDiagnosticFixtureRows(t, f)
	result := readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "clear", result.Permissions.State)
	require.Equal(t, 141, result.Permissions.ExpectedCount)
	require.Empty(t, result.Permissions.Missing)
	require.Equal(t, "clear", result.Reseller.State)
	require.Equal(t, before, seedDiagnosticFixtureRows(t, f))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, `DELETE FROM public.permissions WHERE resource='users' AND action='read';
UPDATE public.permissions SET deleted_at=now() WHERE resource='users' AND action='write';
INSERT INTO public.permissions(resource,action) VALUES('private-fixture-key','private-fixture-key');
INSERT INTO public.permissions(resource,action,deleted_at) VALUES('users','delete',now());`)
	planPGOK(t, err, "prepare generated permission differences")
	before = seedDiagnosticFixtureRows(t, f)
	result = readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "conflict", result.Permissions.State)
	require.Equal(t, []database.SeedDiagnosticPermission{
		{Resource: "users", Action: "read"}, {Resource: "users", Action: "write", DeletedConflict: true},
	}, result.Permissions.Missing)
	require.Equal(t, before, seedDiagnosticFixtureRows(t, f))
	_, err = f.admin.Exec(ctx, "DELETE FROM public.permissions WHERE resource<>'private-fixture-key'")
	planPGOK(t, err, "prepare generated complete missing permission set")
	result = readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "drift", result.Permissions.State)
	require.Len(t, result.Permissions.Missing, 141)
}

func TestSeedDiagnosticPGResellerCountsMatchCanonicalPredicates(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := f.admin.Exec(ctx, `INSERT INTO public.users(id,is_super_admin,is_active) VALUES
('00000000-0000-4000-8000-000000000002',true,true),
('00000000-0000-4000-8000-000000000003',true,false);
INSERT INTO public.organizations(id) VALUES('00000000-0000-4000-8000-000000000004');
INSERT INTO public.organizations(id,deleted_at) VALUES('00000000-0000-4000-8000-000000000005',now());`)
	planPGOK(t, err, "prepare generated reseller differences")
	result := readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "drift", result.Reseller.State)
	require.EqualValues(t, 1, result.Reseller.MissingOwnerMembership)
	require.EqualValues(t, 1, result.Reseller.OrganizationsWithoutReseller)
	_, err = f.admin.Exec(ctx, `INSERT INTO public.reseller_members(reseller_id,user_id,role,is_active)
VALUES('00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002',NULL,NULL)`)
	planPGOK(t, err, "prepare generated invalid owner membership")
	result = readSeedDiagnosticFixture(t, cfg)
	require.EqualValues(t, 0, result.Reseller.MissingOwnerMembership)
	require.EqualValues(t, 1, result.Reseller.InvalidOwnerMembership)
	_, err = f.admin.Exec(ctx, `INSERT INTO public.reseller_members SELECT * FROM public.reseller_members;
UPDATE public.resellers SET status=NULL;`)
	planPGOK(t, err, "prepare generated membership ambiguity")
	before := seedDiagnosticFixtureRows(t, f)
	result = readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "conflict", result.Reseller.State)
	require.EqualValues(t, 1, result.Reseller.DuplicateOwnerMembershipGroup)
	require.EqualValues(t, 2, result.Reseller.InvalidOwnerMembership)
	require.EqualValues(t, 1, result.Reseller.PlatformInactiveOrDeleted)
	require.Equal(t, before, seedDiagnosticFixtureRows(t, f))
	_, err = f.admin.Exec(ctx, "INSERT INTO public.resellers(id,slug,status) VALUES('00000000-0000-4000-8000-000000000006',$1,$2)", database.PlatformResellerSlug, models.ResellerStatusActive)
	planPGOK(t, err, "prepare generated duplicate platform")
	result = readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "conflict", result.Reseller.State)
	require.EqualValues(t, 2, result.Reseller.PlatformRows)
	require.EqualValues(t, 1, result.Reseller.MissingOwnerMembership)
	_, err = f.admin.Exec(ctx, "DELETE FROM public.resellers")
	planPGOK(t, err, "remove generated platform")
	result = readSeedDiagnosticFixture(t, cfg)
	require.Equal(t, "drift", result.Reseller.State)
	require.Zero(t, result.Reseller.PlatformRows)
	require.Zero(t, result.Reseller.MissingOwnerMembership, "absent platform is explicit drift, never clear")
}

func TestSeedDiagnosticPGVisibilityAndNativeOwnerRefusals(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct{ name, before, after string }{
		{"rls", "ALTER TABLE public.permissions ENABLE ROW LEVEL SECURITY", "ALTER TABLE public.permissions DISABLE ROW LEVEL SECURITY"},
		{"force_rls", "ALTER TABLE public.permissions FORCE ROW LEVEL SECURITY", "ALTER TABLE public.permissions NO FORCE ROW LEVEL SECURITY"},
		{"policy", "CREATE POLICY hidden ON public.permissions USING(false)", "DROP POLICY hidden ON public.permissions"},
		{"inheritance", "CREATE TABLE public.permission_child() INHERITS(public.permissions)", "DROP TABLE public.permission_child"},
		{"foreign_owner", "ALTER TABLE public.permissions OWNER TO " + pgx.Identifier{f.runtime}.Sanitize(), "ALTER TABLE public.permissions OWNER TO " + pgx.Identifier{f.owner}.Sanitize()},
		{"missing", "ALTER TABLE public.permissions RENAME TO private_missing", "ALTER TABLE public.private_missing RENAME TO permissions"},
		{"no_select", "REVOKE SELECT ON public.permissions FROM " + pgx.Identifier{f.owner}.Sanitize(), "GRANT SELECT ON public.permissions TO " + pgx.Identifier{f.owner}.Sanitize()},
		{"view", "ALTER TABLE public.permissions RENAME TO private_missing; CREATE VIEW public.permissions AS SELECT * FROM public.private_missing; ALTER VIEW public.permissions OWNER TO " + pgx.Identifier{f.owner}.Sanitize(), "DROP VIEW public.permissions; ALTER TABLE public.private_missing RENAME TO permissions"},
		{"partition", "ALTER TABLE public.permissions RENAME TO private_missing; CREATE TABLE public.permissions(resource text,action text,deleted_at timestamptz) PARTITION BY HASH(resource); ALTER TABLE public.permissions OWNER TO " + pgx.Identifier{f.owner}.Sanitize(), "DROP TABLE public.permissions; ALTER TABLE public.private_missing RENAME TO permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.admin.Exec(ctx, tc.before)
			planPGOK(t, err, "change generated visibility boundary")
			raw, err := collectSeedDiagnostic(ctx, cfg)
			require.Error(t, err)
			require.Nil(t, raw)
			_, err = f.admin.Exec(ctx, tc.after)
			planPGOK(t, err, "restore generated visibility boundary")
		})
	}
	_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEROLE BYPASSRLS REPLICATION")
	planPGOK(t, err, "configure generated native managed owner")
	require.Equal(t, "clear", readSeedDiagnosticFixture(t, cfg).Permissions.State)
	_, err = f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.database}.Sanitize()+" OWNER TO "+pgx.Identifier{f.runtime}.Sanitize())
	planPGOK(t, err, "change generated database owner")
	raw, err := collectSeedDiagnostic(ctx, cfg)
	require.Error(t, err)
	require.Nil(t, raw)
}

func TestSeedDiagnosticPGRepeatableReadAndNoPlannerAdmission(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	parent, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	d, ok := newPlanDeadline(parent, time.Now())
	require.True(t, ok)
	ctx, stop := context.WithDeadline(parent, d.workAt)
	defer stop()
	parsed, err := pgx.ParseConfig(cfg.URL)
	planPGOK(t, err, "parse generated diagnostic config")
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	planPGOK(t, err, "configure generated diagnostic session")
	conn, tx, err := openPlanSession(ctx, observer)
	planPGOK(t, err, "open generated diagnostic session")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	before, err := database.ObserveSeedDiagnostic(ctx, tx)
	planPGOK(t, err, "read generated initial snapshot")
	_, err = f.admin.Exec(ctx, "DELETE FROM public.permissions WHERE resource='users' AND action='read'")
	planPGOK(t, err, "change generated concurrent source row")
	after, err := database.ObserveSeedDiagnostic(ctx, tx)
	planPGOK(t, err, "read generated retained snapshot")
	require.Equal(t, before, after)
	require.Equal(t, "drift", readSeedDiagnosticFixture(t, cfg).Permissions.State)
	// A diagnostic application name and native ownership do not manufacture the
	// planner's separate, live held-lock witness.
	_, err = database.ObservePlanSeedsOnHeldOwner(ctx, tx, database.PlanSeedHeldOwner{})
	require.Error(t, err)
}

func TestSeedDiagnosticPGCancellationAndLockWaitDisposeSession(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	parent, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lock, err := f.admin.Begin(parent)
	planPGOK(t, err, "begin generated blocker")
	defer func() { planPGOK(t, lock.Rollback(parent), "release generated blocker") }()
	_, err = lock.Exec(parent, "LOCK TABLE public.permissions IN ACCESS EXCLUSIVE MODE")
	planPGOK(t, err, "hold generated diagnostic blocker")
	for _, cancelled := range []bool{false, true} {
		ctx, stop := context.WithCancel(parent)
		var timer *time.Timer
		if cancelled {
			timer = time.AfterFunc(50*time.Millisecond, stop)
		}
		start := time.Now()
		raw, err := collectSeedDiagnostic(ctx, cfg)
		stop()
		if timer != nil {
			timer.Stop()
		}
		require.Error(t, err)
		require.Nil(t, raw)
		require.Less(t, time.Since(start), 5*time.Second)
		var remaining int
		_, err = f.admin.Exec(parent, "SELECT pg_catalog.pg_stat_clear_snapshot()")
		planPGOK(t, err, "refresh generated session inventory")
		err = f.admin.QueryRow(parent, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=$1 AND application_name=$2", f.database, database.SeedDiagnosticApplication).Scan(&remaining)
		planPGOK(t, err, "inspect generated diagnostic cleanup")
		require.Zero(t, remaining, "no diagnostic session or idle transaction may survive a refused report")
	}
}

func TestSeedDiagnosticPGNativeAdmissionAndPhysicalWitnessRefusals(t *testing.T) {
	f, cfg := newSeedDiagnosticFixture(t)
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgx.ParseConfig(cfg.URL)
	planPGOK(t, err, "parse generated native owner")
	for _, scenario := range []string{"superuser", "set_role", "read_write", "read_committed", "managed_owner_and_witness"} {
		t.Run(scenario, func(t *testing.T) {
			d, ok := newPlanDeadline(parent, time.Now())
			require.True(t, ok)
			ctx, stop := context.WithDeadline(parent, d.workAt)
			defer stop()
			parsed := owner.Copy()
			if scenario == "superuser" || scenario == "set_role" {
				parsed = f.admin.Config()
			}
			if scenario == "managed_owner_and_witness" {
				_, err := f.admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{f.owner}.Sanitize()+" CREATEROLE BYPASSRLS REPLICATION")
				planPGOK(t, err, "configure generated managed owner admission")
			}
			observer, err := seedDiagnosticConfig(parsed, d, ctx)
			planPGOK(t, err, "configure generated admission session")
			conn, err := pgx.ConnectConfig(ctx, observer)
			planPGOK(t, err, "connect generated admission session")
			opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
			if scenario == "read_write" {
				opts.AccessMode = pgx.ReadWrite
			}
			if scenario == "read_committed" {
				opts.IsoLevel = pgx.ReadCommitted
			}
			tx, err := conn.BeginTx(ctx, opts)
			defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
			planPGOK(t, err, "begin generated admission snapshot")
			if scenario == "set_role" {
				_, err = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{f.owner}.Sanitize())
				planPGOK(t, err, "set generated foreign session role")
			}
			report, err := database.ObserveSeedDiagnostic(ctx, tx)
			if scenario != "managed_owner_and_witness" {
				require.Error(t, err)
				require.False(t, report.CompleteVisibility)
				return
			}
			planPGOK(t, err, "observe generated native managed owner")
			require.Equal(t, "clear", report.Permissions.State)
			_, err = database.ObservePlanSeeds(ctx, tx)
			require.Error(t, err, "diagnostic admission cannot relax generic privileged planner rejection")
			_, err = database.ObservePlanSeedsOnHeldOwner(ctx, tx, database.PlanSeedHeldOwner{})
			require.Error(t, err, "native ownership cannot invent a held migration witness")
			before, err := readPlanSelf(ctx, tx, database.SeedDiagnosticApplication, owner.User, false)
			planPGOK(t, err, "read physical diagnostic identity")
			require.Equal(t, f.database, before.database)
			_, err = readPlanSelf(ctx, tx, database.SeedDiagnosticApplication, f.runtime, false)
			require.Error(t, err, "a different expected login cannot validate the same backend")
			_, err = tx.Exec(ctx, "SELECT pg_catalog.set_config('application_name','changed-fixture-session',true)")
			planPGOK(t, err, "change generated physical witness marker")
			_, err = tx.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()")
			planPGOK(t, err, "refresh generated physical witness")
			_, err = readPlanSelf(ctx, tx, database.SeedDiagnosticApplication, owner.User, false)
			require.Error(t, err, "changed physical session marker cannot publish the old witness")
		})
	}
}
