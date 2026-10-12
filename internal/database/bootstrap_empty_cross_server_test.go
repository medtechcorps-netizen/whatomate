//go:build pr7_bootstrap_two_server

package database_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTenantRLS_CatalogBootstrapCommandEqualOIDsAcrossServersRefusesBeforeWrites(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	// Root must provision two fresh, independently initialized PG17 servers.
	// Missing inputs or coincident identities never become a skipped success.
	rawA, rawB := os.Getenv("TEST_BOOTSTRAP_PAIR_DATABASE_URL_A"), os.Getenv("TEST_BOOTSTRAP_PAIR_DATABASE_URL_B")
	if rawA == "" || rawB == "" {
		t.Fatal("required owned two-server bootstrap fixture is not configured")
	}
	bases := make([]*url.URL, 2)
	for index, raw := range []string{rawA, rawB} {
		base, err := catalogTestURL(raw)
		if err != nil {
			t.Fatal("two-server bootstrap fixture URL refused")
		}
		require.Equal(t, "/postgres", base.Path, "pair fixture must use each disposable server's maintenance database")
		bases[index] = base
	}
	require.NotEqual(t, bases[0].Host, bases[1].Host)
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	nonce := strings.ReplaceAll(id.String(), "-", "")
	name, ownerRole, runtimeRole := "pr7_pair_"+nonce, "pr7_owner_"+nonce, "pr7_runtime_"+nonce
	ownerPassword, runtimePassword := "synthetic_owner_"+nonce, "synthetic_runtime_"+nonce
	type server struct {
		admin, owner, runtime                   *gorm.DB
		systemID                                string
		ownerAttempt, runtimeAttempt, dbAttempt bool
	}
	servers := make([]server, 2)
	// Register before connecting/creating. Exact nonce resources were required
	// absent; attempted creates are reconciled by IF EXISTS on these names only.
	t.Cleanup(func() {
		for index := range servers {
			s := &servers[index]
			for _, db := range []*gorm.DB{s.runtime, s.owner} {
				if catalogClose(db) != nil {
					t.Error("owned pair role pool cleanup failed")
				}
			}
			if s.admin == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			admin := s.admin.WithContext(ctx)
			if s.dbAttempt && admin.Exec("DROP DATABASE IF EXISTS "+name+" WITH (FORCE)").Error != nil {
				t.Error("owned pair database cleanup failed")
			}
			if s.runtimeAttempt && admin.Exec("DROP ROLE IF EXISTS "+runtimeRole).Error != nil {
				t.Error("owned pair runtime role cleanup failed")
			}
			if s.ownerAttempt && admin.Exec("DROP ROLE IF EXISTS "+ownerRole).Error != nil {
				t.Error("owned pair owner role cleanup failed")
			}
			cancel()
			if catalogClose(s.admin) != nil {
				t.Error("owned pair administrative pool cleanup failed")
			}
		}
	})
	for index := range servers {
		s := &servers[index]
		s.admin, err = catalogConnect(bases[index])
		if err != nil {
			t.Fatal("connect to owned pair administrator")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		admin := s.admin.WithContext(ctx)
		var metadata []struct {
			SystemID string
			Version  int
			Safe     bool
		}
		result := admin.Raw(`SELECT control.system_identifier::text AS system_id,
			pg_catalog.current_setting('server_version_num')::integer AS version,
			current_user=session_user AND role.rolsuper AND NOT pg_catalog.pg_is_in_recovery() AS safe
			FROM pg_catalog.pg_control_system() control CROSS JOIN pg_catalog.pg_roles role
			WHERE role.rolname=current_user`).Scan(&metadata)
		require.NoError(t, result.Error)
		require.EqualValues(t, 1, result.RowsAffected)
		require.Len(t, metadata, 1)
		require.Equal(t, 17, metadata[0].Version/10000)
		require.True(t, metadata[0].Safe)
		require.NotEmpty(t, metadata[0].SystemID)
		s.systemID = metadata[0].SystemID
		var existing int64
		require.NoError(t, admin.Raw(`SELECT
			(SELECT COUNT(*) FROM pg_catalog.pg_roles WHERE rolname IN (?,?)) +
			(SELECT COUNT(*) FROM pg_catalog.pg_database WHERE datname=?)`, ownerRole, runtimeRole, name).Scan(&existing).Error)
		require.Zero(t, existing, "never adopt an existing role or database")
	}
	require.NotEqual(t, servers[0].systemID, servers[1].systemID, "different URLs must identify independently initialized servers")
	// Run the identical allocation sequence on both servers. Assert observed
	// OIDs below; never set OIDs, pad an allocator, or treat mismatch as a skip.
	for index := range servers {
		s := &servers[index]
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin := s.admin.WithContext(ctx)
		s.ownerAttempt = true
		if admin.Exec("CREATE ROLE "+ownerRole+" LOGIN PASSWORD '"+ownerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION").Error != nil {
			t.Fatal("create owned pair owner role")
		}
		s.runtimeAttempt = true
		if admin.Exec("CREATE ROLE "+runtimeRole+" LOGIN PASSWORD '"+runtimePassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION").Error != nil {
			t.Fatal("create owned pair runtime role")
		}
		s.dbAttempt = true
		if admin.Exec("CREATE DATABASE "+name+" TEMPLATE template0 OWNER "+ownerRole).Error != nil {
			t.Fatal("create owned pair database")
		}
		databaseURL := *bases[index]
		databaseURL.Path, databaseURL.RawPath = "/"+name, ""
		databaseAdmin, connectErr := catalogConnect(&databaseURL)
		if connectErr != nil {
			t.Fatal("connect to owned pair schema administrator")
		}
		alterErr := databaseAdmin.WithContext(ctx).Exec("ALTER SCHEMA public OWNER TO " + ownerRole).Error
		closeErr := catalogClose(databaseAdmin)
		if alterErr != nil || closeErr != nil {
			t.Fatal("initialize owned pair public schema")
		}
		s.owner, err = catalogConnect(catalogRoleURL(bases[index], name, ownerRole, ownerPassword))
		require.NoError(t, err)
		s.runtime, err = catalogConnect(catalogRoleURL(bases[index], name, runtimeRole, runtimePassword))
		require.NoError(t, err)
		for _, db := range []*gorm.DB{s.owner, s.runtime} {
			pool, poolErr := db.DB()
			require.NoError(t, poolErr)
			pool.SetMaxOpenConns(1)
			pool.SetMaxIdleConns(1)
		}
	}
	type actualIdentity struct {
		DatabaseOID int64 `gorm:"column:database_oid"`
		OwnerOID    int64 `gorm:"column:owner_oid"`
		RuntimeOID  int64 `gorm:"column:runtime_oid"`
		Database    string
	}
	identities := make([]actualIdentity, 2)
	before := make([]string, 2)
	for index := range servers {
		result := servers[index].owner.Raw(`SELECT database.oid::bigint AS database_oid,
			(SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname=?) AS owner_oid,
			(SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname=?) AS runtime_oid,
			database.datname::text AS database
			FROM pg_catalog.pg_database database WHERE database.datname=pg_catalog.current_database()`, ownerRole, runtimeRole).Scan(&identities[index])
		require.NoError(t, result.Error)
		require.EqualValues(t, 1, result.RowsAffected)
		require.Equal(t, name, identities[index].Database)
		require.Positive(t, identities[index].DatabaseOID)
		require.Positive(t, identities[index].OwnerOID)
		require.Positive(t, identities[index].RuntimeOID)
		before[index] = bootstrapCommandEmptyPublicState(t, servers[index].owner)
	}
	require.Equal(t, identities[0], identities[1], "actual equal-OID setup is a prerequisite, never a fabricated input")
	ownerBefore, err := bootstrapCommandReadSession(context.Background(), servers[0].owner.Statement.ConnPool)
	require.NoError(t, err)
	runtimeBefore := bootstrapCommandSetOriginalName(t, servers[1].runtime, "pair-runtime-original")
	// Backend PIDs are local to each server. Keep the first B backend alive
	// while opening a second real candidate; at least one must differ from A.
	originalRuntime := servers[1].runtime
	runtimePool, err := originalRuntime.DB()
	require.NoError(t, err)
	leaseContext, cancelLease := context.WithTimeout(context.Background(), 15*time.Second)
	heldRuntime, err := runtimePool.Conn(leaseContext)
	cancelLease()
	require.NoError(t, err)
	var spareRuntime *gorm.DB
	t.Cleanup(func() {
		if heldRuntime != nil && heldRuntime.Close() != nil {
			t.Error("owned pair held runtime cleanup failed")
		}
		if catalogClose(spareRuntime) != nil {
			t.Error("owned pair spare runtime pool cleanup failed")
		}
	})
	heldBefore, err := bootstrapCommandReadSession(context.Background(), heldRuntime)
	require.NoError(t, err)
	require.Equal(t, runtimeBefore, heldBefore)
	spareRuntime, err = catalogConnect(catalogRoleURL(bases[1], name, runtimeRole, runtimePassword))
	require.NoError(t, err)
	sparePool, err := spareRuntime.DB()
	require.NoError(t, err)
	sparePool.SetMaxOpenConns(1)
	sparePool.SetMaxIdleConns(1)
	candidateBefore := bootstrapCommandSetOriginalName(t, spareRuntime, "pair-runtime-original")
	candidateContext, cancelCandidate := context.WithTimeout(context.Background(), 15*time.Second)
	heldCandidate, err := sparePool.Conn(candidateContext)
	cancelCandidate()
	require.NoError(t, err)
	t.Cleanup(func() {
		if heldCandidate != nil && heldCandidate.Close() != nil {
			t.Error("owned pair held candidate cleanup failed")
		}
	})
	candidateHeld, err := bootstrapCommandReadSession(context.Background(), heldCandidate)
	require.NoError(t, err)
	require.Equal(t, candidateBefore, candidateHeld)
	require.NotEqual(t, runtimeBefore.PID, candidateBefore.PID, "overlapping backends on B must have distinct real PIDs")
	require.Equal(t, runtimeBefore.Role, candidateBefore.Role)
	heldDuring, err := bootstrapCommandReadSession(context.Background(), heldRuntime)
	require.NoError(t, err)
	require.Equal(t, heldBefore, heldDuring, "the first B backend stays live during candidate selection")
	if candidateBefore.PID != ownerBefore.PID {
		require.NoError(t, heldCandidate.Close())
		heldCandidate = nil
		servers[1].runtime, spareRuntime = spareRuntime, originalRuntime
		runtimeBefore = candidateBefore
	} else {
		require.NoError(t, heldRuntime.Close())
		heldRuntime = nil
	}
	t.Logf("real pair PID selection: owner A=%d, B candidates=%d/%d, selected B=%d", ownerBefore.PID, heldBefore.PID, candidateBefore.PID, runtimeBefore.PID)
	require.NotEqual(t, ownerBefore.PID, runtimeBefore.PID, "reach the challenge, not the early coincident-PID refusal")
	var mu sync.Mutex
	attemptedDDL := []string{}
	require.NoError(t, servers[0].owner.Callback().Raw().Before("gorm:raw").Register("pr7:cross_server_no_ddl", func(tx *gorm.DB) {
		statement := strings.ToUpper(strings.TrimSpace(tx.Statement.SQL.String()))
		for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "GRANT ", "REVOKE "} {
			if strings.HasPrefix(statement, prefix) {
				mu.Lock()
				attemptedDDL = append(attemptedDDL, statement)
				mu.Unlock()
			}
		}
	}))
	cfg := &config.Config{App: config.AppConfig{Environment: "test"}, Database: config.DatabaseConfig{
		MigrationURL: catalogRoleURL(bases[0], name, ownerRole, ownerPassword).String(),
		URL:          catalogRoleURL(bases[1], name, runtimeRole, runtimePassword).String(),
		RuntimeRole:  runtimeRole, RLSEnabled: true,
	}, DefaultAdmin: config.DefaultAdminConfig{Email: "pair@example.invalid", Password: "synthetic_pair_admin_only", FullName: "Synthetic Pair"}}
	calls := 0
	err = database.BootstrapEmptyDatabase(servers[0].owner, servers[1].runtime, cfg, "", nil, nil,
		bootstrapCommandRealBackfills(&calls))
	require.EqualError(t, err, "bootstrap paired target challenge mismatch")
	require.Zero(t, calls)
	mu.Lock()
	ddl := append([]string(nil), attemptedDDL...)
	mu.Unlock()
	require.Empty(t, ddl)
	for index := range servers {
		require.Equal(t, before[index], bootstrapCommandEmptyPublicState(t, servers[index].owner))
		var markers int64
		require.NoError(t, servers[index].owner.Raw(`SELECT COUNT(*) FROM pg_catalog.pg_stat_activity
			WHERE datname=? AND application_name LIKE 'rereply-bootstrap-%'`, name).Scan(&markers).Error)
		require.Zero(t, markers)
	}
	runtimeAfter, err := bootstrapCommandReadSession(context.Background(), servers[1].runtime.Statement.ConnPool)
	require.NoError(t, err)
	require.Equal(t, runtimeBefore, runtimeAfter)
}
