package database_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type bootstrapCommandSessionState struct {
	PID             int64
	Started         time.Time
	Role            string
	ApplicationName string
}

// Read directly through the exact supplied ConnPool. Never call DB/Connection
// from inside an observer, which could borrow a different session or deadlock.
func bootstrapCommandReadSession(ctx context.Context, pool gorm.ConnPool) (bootstrapCommandSessionState, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var state bootstrapCommandSessionState
	err := pool.QueryRowContext(ctx, `SELECT pg_catalog.pg_backend_pid()::bigint, activity.backend_start,
		current_user::text, pg_catalog.current_setting('application_name')
		FROM pg_catalog.pg_stat_activity activity WHERE activity.pid=pg_catalog.pg_backend_pid()`).Scan(
		&state.PID, &state.Started, &state.Role, &state.ApplicationName)
	return state, err
}

func bootstrapCommandSetOriginalName(t *testing.T, db *gorm.DB, name string) bootstrapCommandSessionState {
	t.Helper()
	var returned string
	require.NoError(t, db.Raw("SELECT pg_catalog.set_config('application_name', ?, false)", name).Scan(&returned).Error)
	require.Equal(t, name, returned)
	state, err := bootstrapCommandReadSession(context.Background(), db.Statement.ConnPool)
	require.NoError(t, err)
	require.Equal(t, name, state.ApplicationName)
	return state
}

// The contender has its own connection. If it unexpectedly acquires the
// concrete migration lock, release it before reporting the observation.
func bootstrapCommandProbeLock(ctx context.Context, contender *gorm.DB, ownerPID int64) (acquired bool, holders int64, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pool, err := contender.DB()
	if err != nil {
		return false, 0, err
	}
	connection, err := pool.Conn(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { returnErr = errors.Join(returnErr, connection.Close()) }()
	if err := connection.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_catalog.pg_locks
		WHERE locktype='advisory' AND pid=$1 AND granted AND mode='ExclusiveLock'
		AND database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
		AND classid::bigint=(pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
		AND objid=1380270905::oid AND objsubid=2`, ownerPID).Scan(&holders); err != nil {
		return false, 0, err
	}
	if err := connection.QueryRowContext(ctx, `SELECT pg_catalog.pg_try_advisory_lock(
		pg_catalog.hashtext(pg_catalog.current_database()), 1380270905)`).Scan(&acquired); err != nil {
		return false, holders, err
	}
	if acquired {
		var unlocked bool
		if err := connection.QueryRowContext(ctx, `SELECT pg_catalog.pg_advisory_unlock(
			pg_catalog.hashtext(pg_catalog.current_database()), 1380270905)`).Scan(&unlocked); err != nil {
			return true, holders, err
		}
		if !unlocked {
			return true, holders, errors.New("test contender did not release its own lock")
		}
	}
	return acquired, holders, nil
}

func TestTenantRLS_CatalogBootstrapCommandHeldRuntimePinAndOwnerLock(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	f := catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, f)
	originalName := strings.Repeat("x", 60) + "   "
	require.Len(t, []byte(originalName), 63)
	runtimeBefore := bootstrapCommandSetOriginalName(t, f.runtime, originalName)
	ownerBefore, err := bootstrapCommandReadSession(context.Background(), f.owner.Statement.ConnPool)
	require.NoError(t, err)
	contender, err := catalogConnect(catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(contender)) })

	var mu sync.Mutex
	observing := false
	pins := map[*sql.Conn]int{}
	unexpectedPool := false
	verifyCalls := 0
	var observedRuntime, observedOwner bootstrapCommandSessionState
	var observedOwnerPool gorm.ConnPool
	var observationErr error
	var acquiredAtVerify bool
	var holdersAtVerify int64
	observe := func(tx *gorm.DB) {
		mu.Lock()
		if !observing {
			mu.Unlock()
			return
		}
		if connection, ok := tx.Statement.ConnPool.(*sql.Conn); ok {
			pins[connection]++
		} else {
			unexpectedPool = true
		}
		isVerifier := strings.Contains(tx.Statement.SQL.String(), "WITH backend_identity AS (") &&
			strings.Contains(tx.Statement.SQL.String(), "runtime_identity AS (")
		ownerPID := observedOwner.PID
		mu.Unlock()
		if !isVerifier {
			return
		}
		state, stateErr := bootstrapCommandReadSession(tx.Statement.Context, tx.Statement.ConnPool)
		acquired, holders, lockErr := bootstrapCommandProbeLock(tx.Statement.Context, contender, ownerPID)
		mu.Lock()
		verifyCalls++
		observedRuntime = state
		acquiredAtVerify, holdersAtVerify = acquired, holders
		observationErr = errors.Join(observationErr, stateErr, lockErr)
		mu.Unlock()
	}
	// Only this owned handle's existing callback chains are observed. Results,
	// errors and all concrete verifier queries continue unchanged.
	require.NoError(t, f.runtime.Callback().Row().Before("gorm:row").Register("pr7:observe_runtime_row", observe))
	require.NoError(t, f.runtime.Callback().Query().Before("gorm:query").Register("pr7:observe_runtime_query", observe))
	require.NoError(t, f.runtime.Callback().Raw().Before("gorm:raw").Register("pr7:observe_runtime_raw", observe))
	calls := 0
	realBackfills := bootstrapCommandRealBackfills(&calls)
	mu.Lock()
	observing = true
	mu.Unlock()
	err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, "", nil, nil, func(session *gorm.DB) error {
		state, readErr := bootstrapCommandReadSession(session.Statement.Context, session.Statement.ConnPool)
		mu.Lock()
		observedOwner, observedOwnerPool = state, session.Statement.ConnPool
		observationErr = errors.Join(observationErr, readErr)
		mu.Unlock()
		return realBackfills(session)
	})
	mu.Lock()
	observing = false
	mu.Unlock()
	require.NoError(t, err)
	require.NoError(t, observationErr)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, verifyCalls, "real internal runtime verifier must be reached exactly once")
	require.False(t, unexpectedPool)
	require.Len(t, pins, 1, "all observed runtime queries must retain one actual sql.Conn")
	require.IsType(t, &sql.Conn{}, observedOwnerPool)
	require.Equal(t, ownerBefore, observedOwner)
	require.Equal(t, runtimeBefore.PID, observedRuntime.PID)
	require.Equal(t, runtimeBefore.Started, observedRuntime.Started)
	require.Equal(t, catalogTemplate.runtime, observedRuntime.Role)
	require.True(t, strings.HasPrefix(observedRuntime.ApplicationName, "rereply-bootstrap-"))
	require.False(t, acquiredAtVerify)
	require.EqualValues(t, 1, holdersAtVerify)
	after, err := bootstrapCommandReadSession(context.Background(), f.runtime.Statement.ConnPool)
	require.NoError(t, err)
	require.Equal(t, runtimeBefore, after, "same backend must restore all 63 original bytes including spaces")
	acquired, holders, err := bootstrapCommandProbeLock(context.Background(), contender, ownerBefore.PID)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Zero(t, holders)
	for _, db := range []*gorm.DB{f.owner, f.runtime} {
		pool, err := db.DB()
		require.NoError(t, err)
		require.Zero(t, pool.Stats().InUse)
		require.Same(t, pool, db.Statement.ConnPool)
	}
}

// GORM Scan calls the caller's Trace after consuming and closing sql.Rows.
// Delegation preserves ordinary logging/error semantics; only cancellation of
// the test-owned context occurs after the successful real marker readback.
type bootstrapCommandAfterScanLogger struct {
	logger.Interface
	after func(string, int64, error)
}

func (l bootstrapCommandAfterScanLogger) Trace(ctx context.Context, start time.Time, fc func() (string, int64), err error) {
	statement, rows := fc()
	l.after(statement, rows, err)
	l.Interface.Trace(ctx, start, func() (string, int64) { return statement, rows }, err)
}

func TestTenantRLS_CatalogBootstrapCommandCancelledMarkerRestoresBeforePoolDisposal(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	f := catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, f)
	original := bootstrapCommandSetOriginalName(t, f.runtime, "")
	ownerBefore, err := bootstrapCommandReadSession(context.Background(), f.owner.Statement.ConnPool)
	require.NoError(t, err)
	before := bootstrapCommandEmptyPublicState(t, f.owner)
	contender, err := catalogConnect(catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(contender)) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	completedMarkerSets := 0
	f.runtime.Logger = bootstrapCommandAfterScanLogger{Interface: f.runtime.Logger, after: func(statement string, rows int64, err error) {
		if err == nil && rows == 1 && strings.HasPrefix(strings.TrimSpace(statement), "SELECT pg_catalog.set_config('application_name',") &&
			strings.Contains(statement, "'rereply-bootstrap-") {
			once.Do(func() {
				completedMarkerSets++
				cancel()
			})
		}
	}}
	calls := 0
	err = database.BootstrapEmptyDatabase(f.owner.WithContext(ctx), f.runtime.WithContext(ctx), cfg, "", nil, nil,
		bootstrapCommandRealBackfills(&calls))
	require.Equal(t, 1, completedMarkerSets)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "release database migration advisory lock")
	require.ErrorContains(t, err, "restore database migration search path")
	require.NotContains(t, err.Error(), "partial state may remain")
	require.Zero(t, calls)
	require.Equal(t, before, bootstrapCommandEmptyPublicState(t, f.owner))
	after, readErr := bootstrapCommandReadSession(context.Background(), f.runtime.Statement.ConnPool)
	require.NoError(t, readErr)
	require.Equal(t, original, after, "WithoutCancel marker restoration must retain the original backend")
	runtimePool, err := f.runtime.DB()
	require.NoError(t, err)
	require.Zero(t, runtimePool.Stats().InUse)

	// The unchanged owner wrapper attempted cleanup with the cancelled context.
	// Its joined errors are not disposal success. Close the dedicated owner
	// pool, then require real lock release within this test's observation window.
	require.NoError(t, catalogClose(f.owner))
	waitCtx, stopWaiting := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWaiting()
	for {
		acquired, holders, probeErr := bootstrapCommandProbeLock(waitCtx, contender, ownerBefore.PID)
		require.NoError(t, probeErr)
		if acquired && holders == 0 {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("cancelled owner pool retained its migration lock after disposal observation window")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
