package main

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

type migrationPlanFixture struct {
	admin                    *pgx.Conn
	cfg                      config.DatabaseConfig
	database, owner, runtime string
}

// Only this test-only fixture writes. It never adopts or migrates the database
// named by TEST_DATABASE_URL, and rejects URL connection-routing overrides.
func newMigrationPlanFixture(t *testing.T, bootstrap bool) *migrationPlanFixture {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(raw)
	planPGOK(t, err, "parse guarded test URL")
	require.True(t, (u.Scheme == "postgres" || u.Scheme == "postgresql") && u.User != nil && u.Path != "", "PostgreSQL test URL required")
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname(), "loopback test endpoint required")
	query, err := url.ParseQuery(u.RawQuery)
	require.NoError(t, err)
	for key, values := range query {
		require.Contains(t, []string{"sslmode", "connect_timeout", "statement_cache_capacity", "default_query_exec_mode", "default_transaction_read_only"}, key, "connection overrides refused")
		require.Len(t, values, 1)
	}
	query.Set("connect_timeout", "10")
	u.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cluster, err := pgx.Connect(ctx, u.String())
	planPGOK(t, err, "connect synthetic cluster")
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		planPGOK(t, cluster.Close(cleanup), "close synthetic cluster")
	})
	var major int
	planPGOK(t, cluster.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer/10000").Scan(&major), "read PostgreSQL version")
	require.Equal(t, 17, major, "planner integration requires PG17")
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	suffix := strings.ReplaceAll(id.String(), "-", "")
	f := &migrationPlanFixture{database: "plan_db_" + suffix, owner: "plan_owner_" + suffix[:20], runtime: "plan_runtime_" + suffix[:20]}
	ownerPassword, runtimePassword := "synthetic_owner_"+suffix, "synthetic_runtime_"+suffix
	ownerCreated, runtimeCreated, databaseCreated := false, false, false
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if f.admin != nil {
			planPGOK(t, f.admin.Close(cleanup), "close fixture administrator")
		}
		if databaseCreated {
			_, err := cluster.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{f.database}.Sanitize()+" WITH (FORCE)")
			planPGOK(t, err, "drop owned fixture database")
		}
		if runtimeCreated {
			_, err := cluster.Exec(cleanup, "DROP ROLE "+pgx.Identifier{f.runtime}.Sanitize())
			planPGOK(t, err, "drop owned runtime")
		}
		if ownerCreated {
			_, err := cluster.Exec(cleanup, "DROP ROLE "+pgx.Identifier{f.owner}.Sanitize())
			planPGOK(t, err, "drop owned owner")
		}
	})
	// Passwords and identifiers here are solely generated hexadecimal fixture data.
	_, err = cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{f.owner}.Sanitize()+" LOGIN PASSWORD '"+ownerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
	planPGOK(t, err, "create owned owner")
	ownerCreated = true
	_, err = cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{f.runtime}.Sanitize()+" LOGIN PASSWORD '"+runtimePassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION")
	planPGOK(t, err, "create owned runtime")
	runtimeCreated = true
	_, err = cluster.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{f.database}.Sanitize()+" OWNER "+pgx.Identifier{f.owner}.Sanitize())
	planPGOK(t, err, "create owned database")
	databaseCreated = true
	adminURL := *u
	adminURL.Path = "/" + f.database
	adminURL.RawPath = ""
	f.admin, err = pgx.Connect(ctx, adminURL.String())
	planPGOK(t, err, "connect owned administrator")
	_, err = f.admin.Exec(ctx, "ALTER SCHEMA public OWNER TO "+pgx.Identifier{f.owner}.Sanitize())
	planPGOK(t, err, "set owned schema")
	ownerURL, runtimeURL := adminURL, adminURL
	ownerURL.User = url.UserPassword(f.owner, ownerPassword)
	runtimeURL.User = url.UserPassword(f.runtime, runtimePassword)
	f.cfg = config.DatabaseConfig{MigrationURL: ownerURL.String(), URL: runtimeURL.String(), RuntimeRole: f.runtime, RLSEnabled: true, MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: 300}
	if bootstrap {
		ownerCfg := f.cfg
		ownerCfg.URL = f.cfg.MigrationURL
		owner, err := database.NewPostgresWithContext(ctx, &ownerCfg, false)
		planPGOK(t, err, "connect bootstrap owner")
		ownerSQL, err := owner.DB()
		planPGOK(t, err, "bootstrap owner pool")
		defer func() { planPGOK(t, ownerSQL.Close(), "close bootstrap owner") }()
		runtime, err := database.NewPostgresWithContext(ctx, &f.cfg, false)
		planPGOK(t, err, "connect bootstrap runtime")
		runtimeSQL, err := runtime.DB()
		planPGOK(t, err, "bootstrap runtime pool")
		defer func() { planPGOK(t, runtimeSQL.Close(), "close bootstrap runtime") }()
		cfg := &config.Config{App: config.AppConfig{Environment: "test"}, Database: f.cfg, DefaultAdmin: config.DefaultAdminConfig{Email: "plan@example.invalid", Password: "synthetic_plan_admin_only", FullName: "Synthetic Plan"}}
		profile, overlay := dbcatalog.BootstrapEmptyProfileV0()
		log := logf.New(logf.Opts{Writer: io.Discard})
		err = database.BootstrapEmptyDatabase(owner, runtime, cfg, "v0", profile, overlay, func(session *gorm.DB) error {
			if err := handlers.BackfillChatbotFlowGraph(session, log); err != nil {
				return err
			}
			_, err := channelapi.BackfillLegacyWhatsAppInbox(session, 500)
			return err
		})
		planPGOK(t, err, "bootstrap owned complete production-shape fixture")
	}
	return f
}

func planPGOK(t *testing.T, err error, operation string) {
	t.Helper()
	if err != nil {
		t.Fatal(operation + " failed")
	} // never echo a driver DSN or fixture password
}

func holdMigrationPlanLock(t *testing.T, pool *sql.DB) *sql.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	planPGOK(t, err, "lease migration pool")
	t.Cleanup(func() {
		defer func() { planPGOK(t, conn.Close(), "return fixture held lease") }()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		var unlocked bool
		planPGOK(t, conn.QueryRowContext(cleanup, "SELECT pg_advisory_unlock(hashtext(current_database()),1380270905)").Scan(&unlocked), "unlock fixture migration")
		require.True(t, unlocked)
	})
	var locked bool
	planPGOK(t, conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext(current_database()),1380270905)").Scan(&locked), "take fixture migration lock")
	require.True(t, locked)
	return conn
}

func TestMigrationPlanPGConstructorAndHeldTarget(t *testing.T) {
	f := newMigrationPlanFixture(t, false)
	cfg := f.cfg
	ownerURL, err := url.Parse(cfg.MigrationURL)
	require.NoError(t, err)
	q := ownerURL.Query()
	q.Set("TimeZone", "UTC")
	ownerURL.RawQuery = q.Encode()
	cfg.MigrationURL = ownerURL.String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, state, err := openPlannedMigration(ctx, &cfg)
	planPGOK(t, err, "construct marked migration pool")
	pool, err := db.DB()
	planPGOK(t, err, "obtain marked pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close marked pool") })
	require.Equal(t, 2, pool.Stats().MaxOpenConnections)
	held := holdMigrationPlanLock(t, pool)
	var application, timezone, current, session string
	planPGOK(t, held.QueryRowContext(ctx, "SELECT current_setting('application_name'),current_setting('TimeZone'),current_user,session_user").Scan(&application, &timezone, &current, &session), "inspect held configuration")
	require.Equal(t, state.marker, application)
	require.Len(t, application, 54)
	require.Equal(t, "UTC", timezone)
	require.Equal(t, f.owner, current)
	require.Equal(t, current, session)
	// Mutating caller config cannot redirect the already-created state.
	cfg.URL = "not a DSN"
	require.Equal(t, f.cfg.URL, state.runtime.URL)
	d, _ := newPlanDeadline(ctx, time.Now())
	work, stop := context.WithDeadline(ctx, d.workAt)
	defer stop()
	parsed, err := pgx.ParseConfig(f.cfg.URL)
	planPGOK(t, err, "parse observer")
	observer, err := planObserverConfig(parsed, planRuntimeName, d, work)
	planPGOK(t, err, "configure observer")
	conn, tx, err := openPlanSession(work, observer)
	planPGOK(t, err, "connect observer")
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete(), "dispose real observer") }()
	self, err := readPlanSelf(work, tx, planRuntimeName, f.runtime, true)
	planPGOK(t, err, "read real observer self")
	target, err := readPlanHeld(work, tx, self, state.marker, f.owner, f.database, nil)
	planPGOK(t, err, "read real held lock")
	_, err = readPlanHeld(work, tx, self, state.marker, f.owner, f.database, &target)
	planPGOK(t, err, "retain real target")
	wrong := target
	wrong.RoleOID++
	_, err = readPlanHeld(work, tx, self, state.marker, f.owner, f.database, &wrong)
	require.Error(t, err)
	// A second actual marked backend must make the token cardinality ambiguous.
	duplicate := state.owner.Copy()
	duplicate.RuntimeParams["application_name"] = state.marker
	other, err := pgx.ConnectConfig(work, duplicate)
	planPGOK(t, err, "connect duplicate fixture marker")
	_, err = readPlanHeld(work, tx, self, state.marker, f.owner, f.database, &target)
	require.Error(t, err)
	planPGOK(t, other.Close(work), "close duplicate marker")
	select {
	case <-other.PgConn().CleanupDone():
	case <-work.Done():
		t.Fatal("duplicate marker cleanup incomplete")
	}
	_, err = readPlanHeld(work, tx, self, state.marker, f.owner, f.database, &target)
	planPGOK(t, err, "fresh witness after duplicate removal")
}

func TestMigrationPlanPGRuntimeDisposedBeforeOwnerAndPoolLimits(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	for _, limit := range []int{0, 1, 2} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			cfg := f.cfg
			cfg.MaxOpenConns = limit
			relay := newMigrationPlanRelay(t, cfg.MigrationURL)
			cfg.MigrationURL = relay.dsn
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			db, state, err := openPlannedMigration(ctx, &cfg)
			planPGOK(t, err, "construct planner")
			pool, err := db.DB()
			planPGOK(t, err, "obtain planner pool")
			t.Cleanup(func() { planPGOK(t, pool.Close(), "close planner pool") })
			holdMigrationPlanLock(t, pool)
			relay.arm(t)
			if limit == 1 {
				result := state.observe(ctx)
				require.Equal(t, []dbcatalog.PlanIssue{{Component: "owner", Code: "capacity"}}, result.incomplete)
				require.Equal(t, "rereply-plan incomplete", result.line())
				select {
				case <-relay.accepted:
					t.Fatal("capacity-one planner attempted another owner connection")
				default:
				}
			} else {
				// Hold the real owner's next TCP accept, not a relation lock that
				// could also block PostgreSQL's catalog deparsing. No PostgreSQL
				// messages, credentials or query results are inspected or changed.
				barrierUntil := time.Now().Add(12 * time.Second)
				barrierCtx, barrierCancel := context.WithDeadline(ctx, barrierUntil)
				defer barrierCancel()
				resultCh := make(chan planObservation, 1)
				observationDone := make(chan struct{})
				go func() { defer close(observationDone); resultCh <- state.observe(ctx) }()
				defer func() {
					cancel()
					relay.proceed()
					select {
					case <-observationDone:
					case <-time.After(20 * time.Second):
						t.Error("planner goroutine did not finish after test cancellation")
					}
				}()
				select {
				case <-relay.accepted:
				case result := <-resultCh:
					t.Fatalf("planner completed before owner connection barrier: %s; fixed issues=%+v", result.line(), result.incomplete)
				case <-barrierCtx.Done():
					t.Fatal("planner owner connection deadline")
				}
				var runtimeCount, ownerCount int
				planPGOK(t, f.admin.QueryRow(barrierCtx, `SELECT count(*) FILTER (WHERE application_name=$1), count(*) FILTER (WHERE application_name=$2) FROM pg_stat_activity WHERE datname=$3`, planRuntimeName, planOwnerName, f.database).Scan(&runtimeCount, &ownerCount), "inspect real backends at owner connection barrier")
				require.Zero(t, runtimeCount, "runtime backend must be gone before owner connection establishment")
				require.Zero(t, ownerCount, "owner traffic is still held before upstream connection")
				relay.proceed()
				require.True(t, time.Now().Before(barrierUntil), "barrier release stays inside the original planner work deadline")
				select {
				case result := <-resultCh:
					require.Empty(t, result.incomplete)
					require.NotEqual(t, "rereply-plan incomplete", result.line())
				case <-ctx.Done():
					t.Fatal("planner did not return")
				}
			}
			var remaining int
			planPGOK(t, f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name IN ($2,$3)", f.database, planRuntimeName, planOwnerName).Scan(&remaining), "verify observer disposal")
			require.Zero(t, remaining)
			require.Equal(t, "spent", state.observe(ctx).incomplete[0].Code, "no reconnect on repeated callback")
			relay.mu.Lock()
			attempts := relay.attempts
			relay.mu.Unlock()
			if limit == 1 {
				require.Equal(t, 1, attempts, "capacity-one uses only the held pool connection")
			} else {
				require.Equal(t, 2, attempts, "exactly one dedicated owner attempt and no reconnect")
			}
		})
	}
}

func TestMigrationPlanPGCancellationDisposalAndExpiredTail(t *testing.T) {
	f := newMigrationPlanFixture(t, false)
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, _ := newPlanDeadline(parent, time.Now())
	parsed, err := pgx.ParseConfig(f.cfg.URL)
	planPGOK(t, err, "parse cleanup observer")
	cfg, err := planObserverConfig(parsed, planRuntimeName, d, parent)
	planPGOK(t, err, "configure cleanup observer")
	conn, tx, err := openPlanSession(parent, cfg)
	planPGOK(t, err, "open cleanup observer")
	query, stop := context.WithTimeout(parent, 40*time.Millisecond)
	_, err = tx.Exec(query, "SELECT pg_catalog.pg_sleep(10)")
	stop()
	require.Error(t, err, "actual cancellation must interrupt the statement")
	disposal := disposePlanSession(d, conn, tx, false)
	// A query cancellation is not itself a claim about physical cleanup. Record
	// the real outcome, and never print a complete result on a cancelled context.
	require.True(t, conn.IsClosed())
	if disposal.complete() {
		require.True(t, disposal.cleanupDone)
	}
	require.Equal(t, "rereply-plan incomplete", (planObservation{parent: query, deadline: d, result: dbcatalog.PlanResult{LiveSHA256: strings.Repeat("a", 64)}}).line())
	select {
	case <-conn.PgConn().CleanupDone():
	case <-parent.Done():
		t.Fatal("owned cancelled observer still cleaning up")
	}
	conn, tx, err = openPlanSession(parent, cfg)
	planPGOK(t, err, "open committed cleanup observer")
	planPGOK(t, tx.Commit(parent), "commit observer")
	require.True(t, disposePlanSession(d, conn, tx, true).complete())
	conn, tx, err = openPlanSession(parent, cfg)
	planPGOK(t, err, "open expired cleanup observer")
	expired := planDeadline{workAt: time.Now().Add(-3 * time.Second), finalAt: time.Now().Add(-time.Second)}
	require.False(t, disposePlanSession(expired, conn, tx, false).complete(), "no renewed tail after original final deadline")
	select {
	case <-conn.PgConn().CleanupDone():
	case <-parent.Done():
		t.Fatal("expired observer still cleaning up")
	}
}
