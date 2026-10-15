package handlers

// H1 pool-wedge hotfix: unit/integration coverage for withBoundedRootConnection
// and the entry points built on it. Every case runs on its own pool from the
// production constructor database.NewPostgres with an explicit MaxOpenConns,
// so a held connection never affects the shared test pool. Slow cases
// (default budgets) run only with H1_SLOW_TESTS=1 and never under -short.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// h1OpenPool opens a dedicated pool through the production constructor. It
// relies on testutil.SetupTestDB for the skip-without-database rule and for
// the one-time migration of the shared schema.
func h1OpenPool(t *testing.T, maxOpen int) (*gorm.DB, *sql.DB) {
	t.Helper()
	testutil.SetupTestDB(t)
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Scheme, "TEST_DATABASE_URL must be a PostgreSQL URL")
	query := parsed.Query()
	query.Set("application_name", "h1_pool_bound_test")
	// Same as testutil.SetupTestDB: other tests in this package recreate
	// relations, so never keep implicitly prepared statements.
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	parsed.RawQuery = query.Encode()
	db, err := database.NewPostgres(&config.DatabaseConfig{
		URL:             parsed.String(),
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxOpen,
		ConnMaxLifetime: 300,
	}, false)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, sqlDB
}

// h1HoldConnections pins n pooled connections until the returned release runs
// (release is idempotent and also registered as cleanup).
func h1HoldConnections(t *testing.T, sqlDB *sql.DB, n int) func() {
	t.Helper()
	held := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := sqlDB.Conn(ctx)
		cancel()
		require.NoError(t, err)
		held = append(held, conn)
	}
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			for _, conn := range held {
				_ = conn.Close()
			}
		}
	}
	t.Cleanup(release)
	return release
}

func h1RootApp(db *gorm.DB, rls bool) *App {
	return &App{
		DB:     db,
		Log:    testutil.NopLogger(),
		Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rls}},
	}
}

// h1NestedApp is a continuation-execution App: it carries an inbound
// continuation and therefore may already own a pooled connection.
func h1NestedApp(root *App, organizationID uuid.UUID) *App {
	return &App{
		DB:     root.DB,
		Log:    root.Log,
		Config: root.Config,
		root:   root,
		inboundContinuation: &inboundContinuationExecution{
			OrganizationID: organizationID,
		},
	}
}

func h1RequireSlow(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow H1 budget test skipped under -short")
	}
	if os.Getenv("H1_SLOW_TESTS") == "" {
		t.Skip("set H1_SLOW_TESTS=1 to run slow H1 budget tests")
	}
}

func h1RequireWedgeGuard(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wedge guard")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// (a) A root receiver holds nothing: it keeps today's direct, unbounded pool use.
func TestH1BoundedRootConnection_RootReceiverUnchanged(t *testing.T) {
	pool, sqlDB := h1OpenPool(t, 2)
	root := h1RootApp(pool, true)

	invoked := false
	var noContext context.Context // the helper's documented "no caller context" form
	require.NoError(t, root.withBoundedRootConnection(noContext, 0, func(db *gorm.DB) error {
		invoked = true
		connPool, isPool := db.Statement.ConnPool.(*sql.DB)
		require.True(t, isPool, "root receiver must receive the *sql.DB itself, got %T", db.Statement.ConnPool)
		assert.Same(t, sqlDB, connPool)
		return nil
	}))
	assert.True(t, invoked)
}

// (b) A transaction-rooted App passes its own *sql.Tx through; nested
// Transaction calls stay savepoints of that same transaction.
func TestH1BoundedRootConnection_TxRootedPassthrough(t *testing.T) {
	pool, _ := h1OpenPool(t, 2)
	require.NoError(t, pool.Transaction(func(tx *gorm.DB) error {
		var outerTxID int64
		require.NoError(t, tx.Raw("SELECT txid_current()").Scan(&outerTxID).Error)
		app := &App{DB: tx, Log: testutil.NopLogger()}
		app.inboundContinuation = &inboundContinuationExecution{}
		return app.withBoundedRootConnection(context.Background(), 0, func(db *gorm.DB) error {
			assert.Same(t, tx.Statement.ConnPool, db.Statement.ConnPool, "tx-rooted App must reuse its transaction")
			return db.Transaction(func(inner *gorm.DB) error {
				var innerTxID int64
				require.NoError(t, inner.Raw("SELECT txid_current()").Scan(&innerTxID).Error)
				assert.Equal(t, outerTxID, innerTxID, "nested Transaction must be a savepoint")
				return nil
			})
		})
	}))
}

// (c) A nested receiver (continuation App, or an App whose DB is a
// transaction) gets a bounded wait and a retryable "wedge guard" error.
func TestH1BoundedRootConnection_NestedWaitIsBounded(t *testing.T) {
	pool, sqlDB := h1OpenPool(t, 2)
	release := h1HoldConnections(t, sqlDB, 2)
	defer release()
	root := h1RootApp(pool, true)

	shared := testutil.SetupTestDB(t)
	foreignTx := shared.Begin()
	require.NoError(t, foreignTx.Error)
	t.Cleanup(func() { foreignTx.Rollback() })

	receivers := map[string]*App{
		"continuation": h1NestedApp(root, uuid.New()),
		"tx_owner":     {DB: foreignTx, Log: root.Log, Config: root.Config, root: root},
	}
	for name, receiver := range receivers {
		t.Run(name, func(t *testing.T) {
			invoked := false
			started := time.Now()
			err := receiver.withBoundedRootConnection(context.Background(), 300*time.Millisecond, func(*gorm.DB) error {
				invoked = true
				return nil
			})
			elapsed := time.Since(started)
			h1RequireWedgeGuard(t, err)
			assert.False(t, invoked, "run must not be invoked without a connection")
			assert.Less(t, elapsed, time.Second)
			assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond)
		})
	}
	// The caller's own expired context is never relabelled as a wedge guard.
	expired, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := h1NestedApp(root, uuid.New()).withBoundedRootConnection(expired, 5*time.Second, func(*gorm.DB) error {
		return nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "wedge guard")
}

// (d) Only the wait is bounded: once a connection is pinned, the transaction
// outlives the wait budget and commits; the connection is always returned,
// also when run panics.
func TestH1BoundedRootConnection_OnlyWaitIsBounded(t *testing.T) {
	pool, sqlDB := h1OpenPool(t, 2)
	root := h1RootApp(pool, true)
	nested := h1NestedApp(root, uuid.New())

	table := "h1_only_wait_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	shared := testutil.SetupTestDB(t)
	require.NoError(t, shared.Exec("CREATE TABLE "+table+" (id integer PRIMARY KEY)").Error)
	t.Cleanup(func() { shared.Exec("DROP TABLE IF EXISTS " + table) })

	baseline := sqlDB.Stats().InUse
	var insideInUse int
	err := nested.withBoundedRootConnection(context.Background(), 300*time.Millisecond, func(db *gorm.DB) error {
		insideInUse = sqlDB.Stats().InUse
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_sleep(1)").Error; err != nil {
				return err
			}
			return tx.Exec("INSERT INTO " + table + " (id) VALUES (1)").Error
		})
	})
	require.NoError(t, err, "the 300 ms wait budget must not bound the pinned transaction")
	assert.Equal(t, baseline+1, insideInUse)
	assert.Equal(t, baseline, sqlDB.Stats().InUse)
	var committed int64
	require.NoError(t, shared.Raw("SELECT count(*) FROM "+table).Scan(&committed).Error)
	assert.EqualValues(t, 1, committed)

	func() {
		defer func() {
			recovered := recover()
			require.NotNil(t, recovered)
		}()
		_ = nested.withBoundedRootConnection(context.Background(), 300*time.Millisecond, func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				require.NoError(t, tx.Exec("INSERT INTO "+table+" (id) VALUES (2)").Error)
				panic("h1 synthetic panic inside pinned transaction")
			})
		})
	}()
	testutil.AssertEventually(t, func() bool { return sqlDB.Stats().InUse == baseline }, 2*time.Second,
		"a panicking run must return its pinned connection")
	require.NoError(t, shared.Raw("SELECT count(*) FROM "+table).Scan(&committed).Error)
	assert.EqualValues(t, 1, committed, "the panicking transaction must roll back")
}

// (e) Derived budgets: 15 s nested, 45 s inside the AI-attempt fence.
func TestH1BoundedRootConnection_DefaultBudget(t *testing.T) {
	h1RequireSlow(t)
	pool, sqlDB := h1OpenPool(t, 1)
	release := h1HoldConnections(t, sqlDB, 1)
	defer release()
	root := h1RootApp(pool, true)

	nested := h1NestedApp(root, uuid.New())
	var noContext context.Context
	started := time.Now()
	err := nested.withBoundedRootConnection(noContext, 0, func(*gorm.DB) error { return nil })
	elapsed := time.Since(started)
	h1RequireWedgeGuard(t, err)
	assert.GreaterOrEqual(t, elapsed, 14*time.Second)
	assert.LessOrEqual(t, elapsed, 17*time.Second)

	nested.inboundContinuation.attemptGuarded = true
	started = time.Now()
	err = nested.withBoundedRootConnection(context.Background(), 0, func(*gorm.DB) error { return nil })
	elapsed = time.Since(started)
	h1RequireWedgeGuard(t, err)
	assert.GreaterOrEqual(t, elapsed, 44*time.Second)
	assert.LessOrEqual(t, elapsed, 48*time.Second)
}

// (f) The AI-attempt fuse still frees its slot on a pinned connection: pgx
// stdlib has no driver.Validator, so database/sql discards a transaction
// rolled back by its context and closes the *sql.Conn before run returns.
func TestH1BoundedRootConnection_FuseReleasesSlot(t *testing.T) {
	pool, sqlDB := h1OpenPool(t, 2)
	root := h1RootApp(pool, true)
	nested := h1NestedApp(root, uuid.New())

	baseline := sqlDB.Stats().InUse
	var duringTx, afterFuse int
	err := nested.withBoundedRootConnection(context.Background(), time.Second, func(db *gorm.DB) error {
		fuseCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		tx := db.WithContext(fuseCtx).Begin()
		if tx.Error != nil {
			return tx.Error
		}
		if err := tx.Exec("SELECT 1").Error; err != nil {
			return err
		}
		duringTx = sqlDB.Stats().InUse
		time.Sleep(1500 * time.Millisecond)
		afterFuse = sqlDB.Stats().InUse
		tx.Rollback()
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, baseline+1, duringTx)
	assert.Equal(t, baseline, afterFuse, "the expired transaction must release its pinned slot before run returns")
	assert.Equal(t, baseline, sqlDB.Stats().InUse)
}

// (g) WithTenantApp on a root receiver keeps its unbounded wait.
func TestH1BoundedRootConnection_WithTenantAppRootReceiverStillUnbounded(t *testing.T) {
	shared := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, shared)
	pool, sqlDB := h1OpenPool(t, 1)
	release := h1HoldConnections(t, sqlDB, 1)
	root := h1RootApp(pool, true)

	done := make(chan error, 1)
	go func() {
		done <- root.WithTenantApp(organization.ID, func(scoped *App) error {
			var one int
			return scoped.DB.Raw("SELECT 1").Scan(&one).Error
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("root-receiver WithTenantApp returned while the pool was full: %v", err)
	case <-time.After(time.Second):
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("root-receiver WithTenantApp did not proceed after the connection was released")
	}
}

// (h) The action-ledger deadline bounds only the pool wait, never the ledger
// transaction or its COMMIT.
func TestH1BoundedRootConnection_LedgerDeadlineBoundsOnlyWait(t *testing.T) {
	shared := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, shared)
	table := "h1_ledger_wait_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, shared.Exec("CREATE TABLE "+table+" (id integer PRIMARY KEY)").Error)
	t.Cleanup(func() { shared.Exec("DROP TABLE IF EXISTS " + table) })

	t.Run("full_pool_fails_fast", func(t *testing.T) {
		pool, sqlDB := h1OpenPool(t, 1)
		release := h1HoldConnections(t, sqlDB, 1)
		defer release()
		execution := h1NestedApp(h1RootApp(pool, true), organization.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		invoked := false
		started := time.Now()
		err := execution.withIndependentInboundContinuationAction(ctx, organization.ID, func(*gorm.DB) error {
			invoked = true
			return nil
		})
		h1RequireWedgeGuard(t, err)
		assert.False(t, invoked)
		assert.Less(t, time.Since(started), time.Second)
	})

	t.Run("free_pool_commits_after_deadline", func(t *testing.T) {
		for _, rls := range []bool{true, false} {
			pool, _ := h1OpenPool(t, 2)
			execution := h1NestedApp(h1RootApp(pool, rls), organization.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			id := 1
			if !rls {
				id = 2
			}
			err := execution.withIndependentInboundContinuationAction(ctx, organization.ID, func(tx *gorm.DB) error {
				time.Sleep(350 * time.Millisecond) // the caller's deadline has now passed
				if err := tx.Exec("SELECT pg_sleep(0.5)").Error; err != nil {
					return err
				}
				return tx.Exec(fmt.Sprintf("INSERT INTO %s (id) VALUES (%d)", table, id)).Error
			})
			cancel()
			require.NoError(t, err, "rls=%v", rls)
		}
		var committed int64
		require.NoError(t, shared.Raw("SELECT count(*) FROM "+table).Scan(&committed).Error)
		assert.EqualValues(t, 2, committed)
	})

	t.Run("spent_budget_is_a_wedge_guard", func(t *testing.T) {
		pool, _ := h1OpenPool(t, 1)
		execution := h1NestedApp(h1RootApp(pool, true), organization.ID)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		err := execution.withIndependentInboundContinuationAction(ctx, organization.ID, func(*gorm.DB) error {
			return errors.New("must not run")
		})
		h1RequireWedgeGuard(t, err)
	})
}

// /ready fails explicitly within its own 2 s ping budget when the pool is
// saturated instead of leaking one blocked request goroutine per probe.
func TestReadyCheck_FailsFastWhenPoolSaturated(t *testing.T) {
	pool, sqlDB := h1OpenPool(t, 1)
	release := h1HoldConnections(t, sqlDB, 1)
	defer release()
	app := &App{DB: pool, Log: testutil.NopLogger()}

	request := testutil.NewGETRequest(t)
	started := time.Now()
	require.NoError(t, app.ReadyCheck(request))
	assert.Less(t, time.Since(started), 3*time.Second)
	testutil.AssertErrorResponse(t, request, fasthttp.StatusInternalServerError, "Database ping failed")
}
