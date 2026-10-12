package database_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Each refusal owns separate roles and a template0 database. In particular,
// role/parameter faults never modify catalogTemplate's shared LOGIN roles.
type bootstrapAuthorityTarget struct {
	control, admin, owner, runtime                                  *gorm.DB
	name, ownerRole, runtimeRole, auxiliaryRole                     string
	ownerAttempt, runtimeAttempt, auxiliaryAttempt, databaseAttempt bool
	parameterAttempt                                                bool
	cfg                                                             *config.Config
}

func bootstrapAuthorityExec(t *testing.T, db *gorm.DB, statement string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if db.WithContext(ctx).Exec(statement).Error != nil {
		t.Fatal("owned bootstrap authority fixture SQL failed")
	}
}

func bootstrapAuthorityBool(t *testing.T, db *gorm.DB, query string, args ...any) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rows []bool
	result := db.WithContext(ctx).Raw(query, args...).Scan(&rows)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
	require.Len(t, rows, 1)
	return rows[0]
}

func bootstrapAuthorityFixture(t *testing.T) *bootstrapAuthorityTarget {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, raw, "required owned PG17 authority fixture is not configured")
	base, err := catalogTestURL(raw)
	if err != nil {
		t.Fatal("owned bootstrap authority endpoint refused")
	}
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	nonce := strings.ReplaceAll(id.String(), "-", "")
	f := &bootstrapAuthorityTarget{}
	f.control, err = catalogConnect(base)
	if err != nil {
		t.Fatal("connect to owned bootstrap authority administrator")
	}
	f.name, f.ownerRole, f.runtimeRole, f.auxiliaryRole = "pr7_auth_db_"+nonce, "pr7_auth_o_"+nonce, "pr7_auth_r_"+nonce, "pr7_auth_a_"+nonce
	ownerPassword, runtimePassword := "synthetic_owner_"+nonce, "synthetic_runtime_"+nonce
	t.Cleanup(func() {
		// These exact names were checked absent. Mark attempted creates before
		// sending SQL; IF EXISTS reconciles an ambiguous creation only here.
		for _, db := range []*gorm.DB{f.runtime, f.owner} {
			if db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if db.WithContext(ctx).Exec("RESET ROLE").Error != nil {
					t.Error("owned authority session reset failed")
				}
				cancel()
			}
		}
		for _, db := range []*gorm.DB{f.runtime, f.owner, f.admin} {
			if catalogClose(db) != nil {
				t.Error("owned authority pool close failed")
			}
		}
		cleanup := func(statement string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if f.control.WithContext(ctx).Exec(statement).Error != nil {
				t.Error("owned authority resource cleanup failed")
			}
		}
		if f.parameterAttempt {
			cleanup("REVOKE SET ON PARAMETER session_replication_role FROM " + f.runtimeRole)
		}
		if f.databaseAttempt {
			cleanup("DROP DATABASE IF EXISTS " + f.name + " WITH (FORCE)")
		}
		for _, role := range []struct {
			name      string
			attempted bool
		}{
			{f.runtimeRole, f.runtimeAttempt}, {f.ownerRole, f.ownerAttempt}, {f.auxiliaryRole, f.auxiliaryAttempt},
		} {
			if role.attempted {
				cleanup("DROP ROLE IF EXISTS " + role.name)
			}
		}
		var remaining int64
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		check := f.control.WithContext(ctx).Raw(`SELECT
			(SELECT COUNT(*) FROM pg_catalog.pg_database WHERE datname=?) +
			(SELECT COUNT(*) FROM pg_catalog.pg_roles WHERE rolname IN (?,?,?))`, f.name, f.ownerRole, f.runtimeRole, f.auxiliaryRole).Scan(&remaining)
		cancel()
		if check.Error != nil || remaining != 0 {
			t.Error("owned authority resources remain or cleanup could not be verified")
		}
		if catalogClose(f.control) != nil {
			t.Error("owned authority administrator close failed")
		}
	})
	require.True(t, bootstrapAuthorityBool(t, f.control, `SELECT current_user=session_user AND rolsuper
		AND current_setting('server_version_num')::integer/10000=17 AND NOT pg_is_in_recovery()
		FROM pg_catalog.pg_roles WHERE rolname=current_user`))
	require.True(t, bootstrapAuthorityBool(t, f.control, `SELECT
		NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname=?) AND
		NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname IN (?,?,?))`, f.name, f.ownerRole, f.runtimeRole, f.auxiliaryRole))
	f.ownerAttempt = true
	bootstrapAuthorityExec(t, f.control, "CREATE ROLE "+f.ownerRole+" LOGIN PASSWORD '"+ownerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
	f.runtimeAttempt = true
	bootstrapAuthorityExec(t, f.control, "CREATE ROLE "+f.runtimeRole+" LOGIN PASSWORD '"+runtimePassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
	f.auxiliaryAttempt = true
	bootstrapAuthorityExec(t, f.control, "CREATE ROLE "+f.auxiliaryRole+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
	f.databaseAttempt = true
	bootstrapAuthorityExec(t, f.control, "CREATE DATABASE "+f.name+" TEMPLATE template0 OWNER "+f.ownerRole)
	adminURL := *base
	adminURL.Path, adminURL.RawPath = "/"+f.name, ""
	f.admin, err = catalogConnect(&adminURL)
	if err != nil {
		t.Fatal("connect to owned authority database administrator")
	}
	bootstrapAuthorityExec(t, f.admin, "ALTER SCHEMA public OWNER TO "+f.ownerRole)
	ownerURL := catalogRoleURL(base, f.name, f.ownerRole, ownerPassword)
	runtimeURL := catalogRoleURL(base, f.name, f.runtimeRole, runtimePassword)
	f.owner, err = catalogConnect(ownerURL)
	if err != nil {
		t.Fatal("connect to owned authority owner LOGIN")
	}
	f.runtime, err = catalogConnect(runtimeURL)
	if err != nil {
		t.Fatal("connect to owned authority runtime LOGIN")
	}
	for _, db := range []*gorm.DB{f.owner, f.runtime} {
		pool, poolErr := db.DB()
		require.NoError(t, poolErr)
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
	}
	f.cfg = &config.Config{
		App:          config.AppConfig{Environment: "test"},
		Database:     config.DatabaseConfig{URL: runtimeURL.String(), MigrationURL: ownerURL.String(), RuntimeRole: f.runtimeRole, RLSEnabled: true},
		DefaultAdmin: config.DefaultAdminConfig{Email: "authority@example.invalid", Password: "synthetic_authority_admin_only", FullName: "Synthetic Authority"},
	}
	require.True(t, bootstrapAuthorityBool(t, f.owner, `SELECT current_user=session_user AND current_user=?
		AND current_database()=? AND NOT rolsuper AND NOT rolcreaterole AND NOT rolbypassrls AND NOT rolreplication
		FROM pg_catalog.pg_roles WHERE rolname=current_user`, f.ownerRole, f.name))
	require.True(t, bootstrapAuthorityBool(t, f.runtime, `SELECT current_user=session_user AND current_user=?
		AND current_database()=? AND NOT rolsuper AND NOT rolcreaterole AND NOT rolbypassrls AND NOT rolreplication
		FROM pg_catalog.pg_roles WHERE rolname=current_user`, f.runtimeRole, f.name))
	bootstrapCommandEmptyPublicState(t, f.owner)
	return f
}

type bootstrapAuthorityState struct{ Catalog, OwnerSession, RuntimeSession string }

// This is an independent, same-database nonmutation projection. Capture all
// rows/ACLs involved in the adversary, not only the boolean product verdict.
func bootstrapAuthoritySnapshot(t *testing.T, f *bootstrapAuthorityTarget) bootstrapAuthorityState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state bootstrapAuthorityState
	result := f.admin.WithContext(ctx).Raw(`WITH owned_roles AS (
		SELECT * FROM pg_catalog.pg_roles WHERE rolname IN (?,?,?)
	), public_namespace AS (SELECT * FROM pg_catalog.pg_namespace WHERE nspname='public')
	SELECT jsonb_build_object(
		'database',(SELECT jsonb_build_object('oid',oid,'name',datname,'owner',datdba,'acl',datacl::text)
			FROM pg_catalog.pg_database WHERE datname=current_database()),
		'roles',(SELECT jsonb_agg(to_jsonb(r) ORDER BY oid) FROM owned_roles r),
		'memberships',(SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY roleid,member,grantor),'[]') FROM pg_catalog.pg_auth_members m
			WHERE member IN (SELECT oid FROM owned_roles) OR roleid IN (SELECT oid FROM owned_roles)),
		'parameter_acl',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY oid),'[]') FROM pg_catalog.pg_parameter_acl p WHERE parname='session_replication_role'),
		'default_acl',(SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY oid),'[]') FROM pg_catalog.pg_default_acl a WHERE defaclrole IN (SELECT oid FROM owned_roles)),
		'public',(SELECT COALESCE(jsonb_agg(to_jsonb(n) ORDER BY oid),'[]') FROM public_namespace n),
		'relations',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY oid),'[]') FROM pg_catalog.pg_class c WHERE relnamespace IN (SELECT oid FROM public_namespace)),
		'attributes',(SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY attrelid,attnum),'[]') FROM pg_catalog.pg_attribute a
			WHERE attrelid IN (SELECT oid FROM pg_catalog.pg_class WHERE relnamespace IN (SELECT oid FROM public_namespace))),
		'functions',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY oid),'[]') FROM pg_catalog.pg_proc p WHERE pronamespace IN (SELECT oid FROM public_namespace)),
		'types',(SELECT COALESCE(jsonb_agg(to_jsonb(y) ORDER BY oid),'[]') FROM pg_catalog.pg_type y WHERE typnamespace IN (SELECT oid FROM public_namespace)),
		'dependencies',(SELECT COALESCE(jsonb_agg(to_jsonb(d) ORDER BY classid,objid,objsubid,refclassid,refobjid,refobjsubid,deptype),'[]') FROM pg_catalog.pg_depend d
			WHERE refclassid='pg_catalog.pg_namespace'::regclass AND refobjid IN (SELECT oid FROM public_namespace)),
		'metadata_view_acl',(SELECT jsonb_build_object('oid',oid,'owner',relowner,'acl',relacl::text)
			FROM pg_catalog.pg_class WHERE oid='pg_catalog.pg_stat_activity'::regclass)
	)::text`, f.ownerRole, f.runtimeRole, f.auxiliaryRole).Scan(&state.Catalog)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
	for _, session := range []struct {
		db          *gorm.DB
		destination *string
	}{{f.owner, &state.OwnerSession}, {f.runtime, &state.RuntimeSession}} {
		result := session.db.WithContext(ctx).Raw(`SELECT jsonb_build_object(
			'pid',pg_backend_pid(),'current_role',current_user,'session_role',session_user,
			'database',current_database(),'application_name',current_setting('application_name'),
			'search_path',current_setting('search_path'),'lock_timeout',current_setting('lock_timeout'),
			'replication_role',current_setting('session_replication_role'))::text`).Scan(session.destination)
		require.NoError(t, result.Error)
		require.EqualValues(t, 1, result.RowsAffected)
	}
	return state
}

func TestTenantRLS_CatalogBootstrapCommandAuthorityRefusalsBeforeWrites(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	for _, test := range []struct{ name, diagnostic string }{
		{"production", "empty bootstrap requires staging or test"},
		{"owner-superuser", "bootstrap session authentication or identity refused"},
		{"runtime-superuser", "bootstrap session authentication or identity refused"},
		{"runtime-set-role", "bootstrap session metadata row count"},
		{"runtime-createrole", "bootstrap runtime privilege refused"},
		{"runtime-bypassrls", "bootstrap runtime privilege refused"},
		{"runtime-replication", "bootstrap runtime privilege refused"},
		{"runtime-owner-membership", "bootstrap runtime inherits owner"},
		{"runtime-privileged-membership", "bootstrap runtime has privileged membership"},
		{"runtime-parameter-set", "bootstrap runtime replication setting authority"},
		{"dangerous-default-acl", "dangerous default table privilege grant(s)"},
		{"missing-public", "database migration search path is"},
		{"wrong-public-owner", "bootstrap owner/schema authority or role inheritance refused"},
		{"missing-owner-create", "bootstrap owner/schema authority or role inheritance refused"},
		{"wrong-database-owner", "bootstrap owner/schema authority or role inheritance refused"},
		{"other-role-inherits-owner", "bootstrap owner/schema authority or role inheritance refused"},
		{"other-role-inherits-runtime", "bootstrap owner/schema authority or role inheritance refused"},
		{"metadata-sql-denied", "bootstrap session metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := bootstrapAuthorityFixture(t)
			switch test.name {
			case "production":
				f.cfg.App.Environment = "production"
				require.Equal(t, "production", f.cfg.App.Environment)
			case "owner-superuser", "runtime-superuser":
				role := f.ownerRole
				if test.name == "runtime-superuser" {
					role = f.runtimeRole
				}
				bootstrapAuthorityExec(t, f.admin, "ALTER ROLE "+role+" SUPERUSER")
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=?", role))
			case "runtime-set-role":
				bootstrapAuthorityExec(t, f.admin, "GRANT "+f.ownerRole+" TO "+f.runtimeRole)
				bootstrapAuthorityExec(t, f.runtime, "SET ROLE "+f.ownerRole)
				require.True(t, bootstrapAuthorityBool(t, f.runtime, "SELECT current_user=? AND session_user=? AND current_user<>session_user", f.ownerRole, f.runtimeRole))
				// The authenticated self row remains visible after SET ROLE, but
				// current-role statistics permissions hide required identity fields.
				// Prove the earlier metadata refusal without assuming which field.
				require.True(t, bootstrapAuthorityBool(t, f.runtime, `SELECT COUNT(*)=1
					AND bool_and(a.usesysid=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=session_user))
					AND bool_and(a.usename=session_user AND a.datname=current_database())
					AND bool_and(a.backend_start IS NULL OR a.application_name IS NULL OR a.backend_type IS NULL)
					FROM pg_catalog.pg_stat_activity a WHERE a.pid=pg_catalog.pg_backend_pid()`))
			case "runtime-createrole", "runtime-bypassrls", "runtime-replication":
				attribute := map[string]string{"runtime-createrole": "CREATEROLE", "runtime-bypassrls": "BYPASSRLS", "runtime-replication": "REPLICATION"}[test.name]
				column := map[string]string{"runtime-createrole": "rolcreaterole", "runtime-bypassrls": "rolbypassrls", "runtime-replication": "rolreplication"}[test.name]
				bootstrapAuthorityExec(t, f.admin, "ALTER ROLE "+f.runtimeRole+" "+attribute)
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT "+column+" FROM pg_catalog.pg_roles WHERE rolname=?", f.runtimeRole))
			case "runtime-owner-membership":
				bootstrapAuthorityExec(t, f.admin, "GRANT "+f.ownerRole+" TO "+f.runtimeRole)
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT pg_has_role(CAST(? AS name),CAST(? AS name),'MEMBER')", f.runtimeRole, f.ownerRole))
			case "runtime-privileged-membership":
				bootstrapAuthorityExec(t, f.admin, "ALTER ROLE "+f.auxiliaryRole+" BYPASSRLS")
				bootstrapAuthorityExec(t, f.admin, "GRANT "+f.auxiliaryRole+" TO "+f.runtimeRole)
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT rolbypassrls AND pg_has_role(CAST(? AS name),oid,'MEMBER') FROM pg_roles WHERE rolname=?", f.runtimeRole, f.auxiliaryRole))
			case "runtime-parameter-set":
				require.False(t, bootstrapAuthorityBool(t, f.admin, "SELECT has_parameter_privilege(CAST(? AS name),'session_replication_role','SET')", f.runtimeRole))
				f.parameterAttempt = true
				bootstrapAuthorityExec(t, f.admin, "GRANT SET ON PARAMETER session_replication_role TO "+f.runtimeRole)
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT has_parameter_privilege(CAST(? AS name),'session_replication_role','SET')", f.runtimeRole))
			case "dangerous-default-acl":
				bootstrapAuthorityExec(t, f.admin, "ALTER DEFAULT PRIVILEGES FOR ROLE "+f.ownerRole+" IN SCHEMA public GRANT TRIGGER ON TABLES TO "+f.runtimeRole)
				require.True(t, bootstrapAuthorityBool(t, f.admin, `SELECT EXISTS (SELECT 1 FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) a
					WHERE d.defaclrole=(SELECT oid FROM pg_roles WHERE rolname=?) AND d.defaclnamespace='public'::regnamespace
					AND a.grantee=(SELECT oid FROM pg_roles WHERE rolname=?) AND a.privilege_type='TRIGGER')`, f.ownerRole, f.runtimeRole))
			case "missing-public":
				bootstrapAuthorityExec(t, f.admin, "DROP SCHEMA public")
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='public')"))
			case "wrong-public-owner":
				bootstrapAuthorityExec(t, f.admin, "ALTER SCHEMA public OWNER TO "+f.auxiliaryRole)
				bootstrapAuthorityExec(t, f.admin, "GRANT USAGE, CREATE ON SCHEMA public TO "+f.ownerRole)
				require.True(t, bootstrapAuthorityBool(t, f.owner, "SELECT has_schema_privilege('public','USAGE') AND has_schema_privilege('public','CREATE') AND NOT pg_has_role(current_user,nspowner,'MEMBER') FROM pg_namespace WHERE nspname='public'"))
			case "missing-owner-create":
				bootstrapAuthorityExec(t, f.admin, "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
				bootstrapAuthorityExec(t, f.admin, "REVOKE CREATE ON SCHEMA public FROM "+f.ownerRole)
				require.True(t, bootstrapAuthorityBool(t, f.owner, "SELECT has_schema_privilege('public','USAGE') AND NOT has_schema_privilege('public','CREATE') AND pg_has_role(current_user,nspowner,'MEMBER') FROM pg_namespace WHERE nspname='public'"))
			case "wrong-database-owner":
				bootstrapAuthorityExec(t, f.admin, "ALTER DATABASE "+f.name+" OWNER TO "+f.auxiliaryRole)
				require.True(t, bootstrapAuthorityBool(t, f.owner, "SELECT datdba<>(SELECT oid FROM pg_roles WHERE rolname=current_user) FROM pg_database WHERE datname=current_database()"))
			case "other-role-inherits-owner", "other-role-inherits-runtime":
				role := f.ownerRole
				if test.name == "other-role-inherits-runtime" {
					role = f.runtimeRole
				}
				bootstrapAuthorityExec(t, f.admin, "GRANT "+role+" TO "+f.auxiliaryRole)
				require.True(t, bootstrapAuthorityBool(t, f.admin, "SELECT pg_has_role(CAST(? AS name),CAST(? AS name),'MEMBER')", f.auxiliaryRole, role))
			case "metadata-sql-denied":
				require.True(t, bootstrapAuthorityBool(t, f.owner, "SELECT has_table_privilege('pg_catalog.pg_stat_activity','SELECT')"))
				// Relation ACLs are database-local. This affects only this newly
				// owned database, never the host or another test database.
				bootstrapAuthorityExec(t, f.admin, "REVOKE SELECT ON pg_catalog.pg_stat_activity FROM PUBLIC")
				require.False(t, bootstrapAuthorityBool(t, f.owner, "SELECT has_table_privilege('pg_catalog.pg_stat_activity','SELECT')"))
				var count int64
				probeErr := f.owner.Raw("SELECT COUNT(*) FROM pg_catalog.pg_stat_activity").Scan(&count).Error
				var serverError *pgconn.PgError
				require.ErrorAs(t, probeErr, &serverError)
				require.Equal(t, "42501", serverError.Code)
			default:
				t.Fatal("unrecognized authority adversary")
			}
			before := bootstrapAuthoritySnapshot(t, f)
			var mu sync.Mutex
			attempts, statements := 0, 0
			observer := func(statement string, _ int64, _ error) {
				fields := strings.Fields(statement)
				mu.Lock()
				defer mu.Unlock()
				statements++
				// Observe after the real call, including a failed DDL attempt.
				// Only session/catalog reads are permitted before the refusal.
				if len(fields) == 0 || (strings.ToUpper(fields[0]) != "SELECT" && strings.ToUpper(fields[0]) != "SHOW" && strings.ToUpper(fields[0]) != "WITH") {
					attempts++
				}
			}
			owner := f.owner.Session(&gorm.Session{Logger: bootstrapCommandAfterScanLogger{Interface: f.owner.Logger, after: observer}})
			runtime := f.runtime.Session(&gorm.Session{Logger: bootstrapCommandAfterScanLogger{Interface: f.runtime.Logger, after: observer}})
			calls := 0
			err := database.BootstrapEmptyDatabase(owner, runtime, f.cfg, "", nil, nil, bootstrapCommandRealBackfills(&calls))
			require.ErrorContains(t, err, test.diagnostic)
			require.NotContains(t, err.Error(), "partial state may remain", "refusal must precede the persistent-phase boundary")
			require.Zero(t, calls)
			mu.Lock()
			observedStatements, observedAttempts := statements, attempts
			mu.Unlock()
			require.Zero(t, observedAttempts, "no attempted bootstrap DDL/DML/grant is allowed")
			if test.name == "production" {
				require.Zero(t, observedStatements)
			} else {
				require.Positive(t, observedStatements)
			}
			if test.name == "metadata-sql-denied" {
				var serverError *pgconn.PgError
				require.True(t, errors.As(err, &serverError), "retain the real metadata permission error")
				require.Equal(t, "42501", serverError.Code)
			}
			require.Equal(t, before, bootstrapAuthoritySnapshot(t, f), "roles, grants, schema/objects and pinned session state must not change")
			for _, db := range []*gorm.DB{f.owner, f.runtime} {
				pool, poolErr := db.DB()
				require.NoError(t, poolErr)
				require.Zero(t, pool.Stats().InUse)
			}
		})
	}
}
