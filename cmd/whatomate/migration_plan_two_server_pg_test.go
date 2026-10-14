//go:build pr7_bootstrap_two_server

package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
)

// Root owns the two PG17 servers. Run sequentially after the bootstrap pair
// test, never as concurrent package tests against the same OID allocators.
func TestMigrationPlanPGEqualOIDsAcrossServersRefusesBeforeOwner(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bases := make([]*url.URL, 2)
	for i, key := range []string{"TEST_BOOTSTRAP_PAIR_DATABASE_URL_A", "TEST_BOOTSTRAP_PAIR_DATABASE_URL_B"} {
		raw := os.Getenv(key)
		if raw == "" {
			t.Fatal("required owned two-server planner fixture is not configured")
		}
		u, err := url.Parse(raw)
		planPGOK(t, err, "parse owned pair URL")
		require.True(t, (u.Scheme == "postgres" || u.Scheme == "postgresql") && u.User != nil && u.Path == "/postgres" && u.Fragment == "", "owned maintenance URL required")
		require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname(), "loopback pair endpoint required")
		query, err := url.ParseQuery(u.RawQuery)
		planPGOK(t, err, "parse owned pair options")
		for key, values := range query {
			require.Contains(t, []string{"sslmode", "connect_timeout", "statement_cache_capacity", "default_query_exec_mode"}, key, "connection routing overrides refused")
			require.Len(t, values, 1)
		}
		query.Set("connect_timeout", "10")
		u.RawQuery = query.Encode()
		bases[i] = u
	}
	require.NotEqual(t, bases[0].Host, bases[1].Host)
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	nonce := strings.ReplaceAll(id.String(), "-", "")
	name, owner, runtime := "pr18_pair_"+nonce, "pr18_owner_"+nonce, "pr18_runtime_"+nonce
	ownerPassword, runtimePassword := "synthetic_owner_"+nonce, "synthetic_runtime_"+nonce
	type server struct {
		cluster, admin                                *pgx.Conn
		systemID                                      string
		ownerAttempt, runtimeAttempt, databaseAttempt bool
	}
	servers := make([]server, 2)
	// Register before creating anything. Reconcile only absent-checked nonce
	// resources, including a CREATE whose acknowledgement was unavailable.
	t.Cleanup(func() {
		for i := range servers {
			s := &servers[i]
			cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
			if s.admin != nil && s.admin.Close(cleanup) != nil {
				t.Error("owned pair administrator cleanup failed")
			}
			if s.cluster != nil {
				for _, item := range []struct {
					attempted bool
					statement string
				}{
					{s.databaseAttempt, "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"},
					{s.runtimeAttempt, "DROP ROLE IF EXISTS " + pgx.Identifier{runtime}.Sanitize()},
					{s.ownerAttempt, "DROP ROLE IF EXISTS " + pgx.Identifier{owner}.Sanitize()},
				} {
					if item.attempted {
						if _, err := s.cluster.Exec(cleanup, item.statement); err != nil {
							t.Error("owned pair resource cleanup failed")
						}
					}
				}
				if s.cluster.Close(cleanup) != nil {
					t.Error("owned pair cluster cleanup failed")
				}
			}
			done()
		}
	})
	for i := range servers {
		s := &servers[i]
		s.cluster, err = pgx.Connect(ctx, bases[i].String())
		planPGOK(t, err, "connect owned pair cluster")
		var major int
		var safe bool
		planPGOK(t, s.cluster.QueryRow(ctx, `SELECT control.system_identifier::text,
			pg_catalog.current_setting('server_version_num')::integer/10000,
			current_user=session_user AND role.rolsuper AND NOT pg_catalog.pg_is_in_recovery()
			FROM pg_catalog.pg_control_system() control CROSS JOIN pg_catalog.pg_roles role
			WHERE role.rolname=current_user`).Scan(&s.systemID, &major, &safe), "inspect owned pair cluster")
		require.Equal(t, 17, major)
		require.True(t, safe)
		require.NotEmpty(t, s.systemID)
		var existing int
		planPGOK(t, s.cluster.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname IN ($1,$2)) +
			(SELECT count(*) FROM pg_catalog.pg_database WHERE datname=$3)`, owner, runtime, name).Scan(&existing), "check owned pair names absent")
		require.Zero(t, existing, "never adopt an existing role or database")
	}
	require.NotEqual(t, servers[0].systemID, servers[1].systemID, "independently initialized servers required")
	// Match the bootstrap pair's allocation order. Observe the resulting OIDs;
	// never pad an allocator, inject an OID or skip a mismatch.
	for i := range servers {
		s := &servers[i]
		s.ownerAttempt = true
		_, err = s.cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{owner}.Sanitize()+" LOGIN PASSWORD '"+ownerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
		planPGOK(t, err, "create owned pair owner")
		s.runtimeAttempt = true
		_, err = s.cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{runtime}.Sanitize()+" LOGIN PASSWORD '"+runtimePassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
		planPGOK(t, err, "create owned pair runtime")
		s.databaseAttempt = true
		_, err = s.cluster.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0 OWNER "+pgx.Identifier{owner}.Sanitize())
		planPGOK(t, err, "create owned pair database")
		u := *bases[i]
		u.Path, u.RawPath = "/"+name, ""
		s.admin, err = pgx.Connect(ctx, u.String())
		planPGOK(t, err, "connect owned pair database administrator")
		_, err = s.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO "+pgx.Identifier{owner}.Sanitize())
		planPGOK(t, err, "set owned pair schema")
	}
	type identity struct {
		Database, Owner, Runtime int64
		Name                     string
		Safe                     bool
	}
	identities := make([]identity, 2)
	before := make([]string, 2)
	for i := range servers {
		v := &identities[i]
		planPGOK(t, servers[i].admin.QueryRow(ctx, `SELECT d.oid::bigint,o.oid::bigint,r.oid::bigint,d.datname,
			NOT(o.rolsuper OR o.rolcreatedb OR o.rolcreaterole OR o.rolinherit OR o.rolbypassrls OR o.rolreplication
			OR r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolinherit OR r.rolbypassrls OR r.rolreplication)
			AND o.rolcanlogin AND r.rolcanlogin
			FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.pg_roles o CROSS JOIN pg_catalog.pg_roles r
			WHERE d.datname=pg_catalog.current_database() AND o.rolname=$1 AND r.rolname=$2`, owner, runtime).Scan(&v.Database, &v.Owner, &v.Runtime, &v.Name, &v.Safe), "read actual pair identities")
		require.Positive(t, v.Database)
		require.Positive(t, v.Owner)
		require.Positive(t, v.Runtime)
		require.Equal(t, name, v.Name)
		require.True(t, v.Safe)
		before[i] = migrationPlanPairState(t, ctx, servers[i].admin, owner, runtime)
	}
	require.Equal(t, identities[0], identities[1], "equal observed database and role OIDs are an actual prerequisite")
	ownerURL, runtimeURL := *bases[0], *bases[1]
	ownerURL.Path, ownerURL.RawPath = "/"+name, ""
	runtimeURL.Path, runtimeURL.RawPath = "/"+name, ""
	ownerURL.User, runtimeURL.User = url.UserPassword(owner, ownerPassword), url.UserPassword(runtime, runtimePassword)
	relay := newMigrationPlanRelay(t, ownerURL.String())
	cfg := config.DatabaseConfig{MigrationURL: relay.dsn, URL: runtimeURL.String(), RuntimeRole: runtime, RLSEnabled: true, MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: 300}
	db, state, err := openPlannedMigration(ctx, &cfg)
	planPGOK(t, err, "construct actual cross-server planner")
	pool, err := db.DB()
	planPGOK(t, err, "obtain actual held pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close actual held pool") })
	held := holdMigrationPlanLock(t, pool)
	readHeld := func() string {
		var raw string
		planPGOK(t, held.QueryRowContext(ctx, `SELECT pg_catalog.jsonb_build_object(
			'pid',a.pid,'backend_start',a.backend_start,'database_oid',a.datid,'role_oid',a.usesysid,
			'database',current_database(),'current_user',current_user,'session_user',session_user,
			'application',current_setting('application_name'),'search_path',current_setting('search_path'),
			'read_only',current_setting('transaction_read_only'),'isolation',current_setting('transaction_isolation'),
			'statement_timeout',current_setting('statement_timeout'),'lock_timeout',current_setting('lock_timeout'),
			'idle_timeout',current_setting('idle_in_transaction_session_timeout'))::text
			FROM pg_catalog.pg_stat_activity a WHERE a.pid=pg_catalog.pg_backend_pid()`).Scan(&raw), "read actual held session")
		return raw
	}
	heldBefore := readHeld()
	checkWitnesses := func() {
		for i := range servers {
			_, err := servers[i].admin.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()")
			planPGOK(t, err, "refresh pair backend observations")
			var raw []byte
			planPGOK(t, servers[i].admin.QueryRow(ctx, planHeldSQL, state.marker).Scan(&raw), "read actual pair held witnesses")
			var witness planWitness
			require.NoError(t, json.Unmarshal(raw, &witness))
			if i == 0 {
				require.EqualValues(t, 1, witness.TokenRows)
				require.EqualValues(t, 1, witness.LockRows)
				require.EqualValues(t, 1, witness.JoinedRows)
				require.NotNil(t, witness.Held)
				require.Equal(t, identities[0].Database, witness.Held.DatabaseOID)
				require.Equal(t, identities[0].Owner, witness.Held.RoleOID)
				require.Equal(t, state.marker, witness.Held.ApplicationName)
			} else {
				require.Zero(t, witness.TokenRows)
				require.Zero(t, witness.LockRows)
				require.Zero(t, witness.JoinedRows)
				require.Nil(t, witness.Held)
			}
		}
	}
	checkWitnesses()
	relay.arm(t)
	started := time.Now()
	result := state.observe(ctx)
	require.Less(t, time.Since(started), 17*time.Second, "original complete planner budget is not extended")
	require.Equal(t, []dbcatalog.PlanIssue{{Component: "runtime", Code: "target"}}, result.incomplete)
	require.Equal(t, "rereply-plan incomplete", result.line())
	require.Equal(t, []dbcatalog.PlanIssue{{Component: "planner", Code: "spent"}}, state.observe(ctx).incomplete)
	relay.mu.Lock()
	attempts := relay.attempts
	relay.mu.Unlock()
	require.Equal(t, 1, attempts, "cross-server refusal must precede any dedicated owner admission")
	select {
	case <-relay.accepted:
		t.Fatal("cross-server refusal attempted owner admission")
	default:
	}
	require.Equal(t, heldBefore, readHeld(), "held backend identity and session settings remain unchanged")
	checkWitnesses()
	for i := range servers {
		var remaining int
		planPGOK(t, servers[i].admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE datname=$1 AND application_name IN ($2,$3)`, name, planRuntimeName, planOwnerName).Scan(&remaining), "verify actual observer disposal")
		require.Zero(t, remaining)
		require.Equal(t, before[i], migrationPlanPairState(t, ctx, servers[i].admin, owner, runtime), "pair catalog and authority stay unchanged")
	}
}

// No public relations/routines/types exist. Compare the real empty namespace,
// ACL and two-role metadata; no observation can silently turn absence into data.
func migrationPlanPairState(t *testing.T, ctx context.Context, conn *pgx.Conn, owner, runtime string) string {
	t.Helper()
	var objects int
	planPGOK(t, conn.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public') +
		(SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public') +
		(SELECT count(*) FROM pg_catalog.pg_type ty JOIN pg_catalog.pg_namespace n ON n.oid=ty.typnamespace WHERE n.nspname='public')`).Scan(&objects), "check real empty pair namespace")
	require.Zero(t, objects)
	var raw string
	planPGOK(t, conn.QueryRow(ctx, `SELECT pg_catalog.jsonb_build_object(
		'database',pg_catalog.to_jsonb(d),'schema',pg_catalog.to_jsonb(n),
		'roles',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(r) ORDER BY r.oid) FROM pg_catalog.pg_roles r WHERE r.rolname IN ($1,$2)),
		'memberships',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(m) ORDER BY m.oid),'[]'::jsonb) FROM pg_catalog.pg_auth_members m
			WHERE m.roleid IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ($1,$2)) OR m.member IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ($1,$2))),
		'default_acl',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.to_jsonb(a) ORDER BY a.oid),'[]'::jsonb) FROM pg_catalog.pg_default_acl a
			WHERE a.defaclrole IN (SELECT oid FROM pg_catalog.pg_roles WHERE rolname IN ($1,$2))))::text
		FROM pg_catalog.pg_database d CROSS JOIN pg_catalog.pg_namespace n
		WHERE d.datname=pg_catalog.current_database() AND n.nspname='public'`, owner, runtime).Scan(&raw), "capture pair catalog and authority")
	return raw
}
