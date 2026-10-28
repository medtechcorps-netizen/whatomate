package handlers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/logf"
)

type poolDiagnosticsLog struct {
	mu     sync.Mutex
	lines  []string
	events chan string
}

func (l *poolDiagnosticsLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.lines = append(l.lines, string(p))
	l.mu.Unlock()
	if l.events != nil {
		l.events <- string(p)
	}
	return len(p), nil
}

func (l *poolDiagnosticsLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func awaitPoolDiagnostic(t *testing.T, logs *poolDiagnosticsLog, reason string) string {
	t.Helper()
	select {
	case line := <-logs.events:
		require.Contains(t, line, "reason="+reason)
		return line
	case <-time.After(time.Second):
		t.Fatal("pool diagnostic was not observed")
		return ""
	}
}

func TestDatabasePoolDiagnosticsCadenceTransitionsAndRecovery(t *testing.T) {
	logs := &poolDiagnosticsLog{}
	logger := logf.New(logf.Opts{Writer: logs, Level: logf.InfoLevel})
	start := time.Unix(1000, 0)
	var state databasePoolDiagnosticsState
	healthy := databasePoolSample{maxOpen: 2, open: 2, idle: 2, waitCount: 10, waitDuration: time.Second}
	state.observe(start, healthy, logger)
	state.observe(start.Add(time.Second), healthy, logger)
	full := healthy
	full.inUse, full.idle = 2, 0
	state.observe(start.Add(2*time.Second), full, logger)
	require.Len(t, logs.snapshot(), 1, "full but no new waits is not a saturation warning")
	full.waitCount++
	state.observe(start.Add(3*time.Second), full, logger)
	require.Len(t, logs.snapshot(), 2, "startup summary must not delay the first saturation warning")
	full.waitCount++
	state.observe(start.Add(4*time.Second), full, logger)
	healthy.waitCount, healthy.waitDuration = full.waitCount, 1500*time.Millisecond
	state.observe(start.Add(5*time.Second), healthy, logger)
	state.observe(start.Add(17*time.Second), healthy, logger)
	require.Len(t, logs.snapshot(), 2, "recovery is coalesced until the transition interval")
	state.observe(start.Add(18*time.Second), healthy, logger)
	state.observe(start.Add(77*time.Second), healthy, logger)
	require.Len(t, logs.snapshot(), 3)
	state.observe(start.Add(78*time.Second), healthy, logger)
	lines := logs.snapshot()
	require.Len(t, lines, 4)
	require.Contains(t, lines[0], "reason=startup")
	for _, field := range []string{"reason=saturated", "max_open=2", "open=2", "in_use=2", "idle=0", "wait_count=11", "wait_count_delta=1", "waits_in_sample=1", "at_capacity=true", "saturated=true"} {
		require.Contains(t, lines[1], field)
	}
	for _, field := range []string{"reason=recovered", "wait_count_delta=1", "wait_duration_delta=500ms", "at_capacity=false", "saturated=false"} {
		require.Contains(t, lines[2], field)
	}
	require.Contains(t, lines[3], "reason=periodic")
}

func TestDatabasePoolDiagnosticsUnlimitedPoolAndHistoricalWaitsAreNotWarnings(t *testing.T) {
	for _, maximum := range []int{0, 2} {
		t.Run(map[int]string{0: "unlimited", 2: "historical_waits"}[maximum], func(t *testing.T) {
			logs := &poolDiagnosticsLog{}
			logger := logf.New(logf.Opts{Writer: logs, Level: logf.WarnLevel})
			var state databasePoolDiagnosticsState
			start := time.Unix(1000, 0)
			sample := databasePoolSample{maxOpen: maximum, open: 2, inUse: 2, waitCount: 99, waitDuration: time.Hour}
			state.observe(start, sample, logger)
			if maximum == 0 {
				sample.waitCount++
			}
			state.observe(start.Add(time.Minute), sample, logger)
			require.Empty(t, logs.snapshot(), "healthy/historical samples must stay at Info level")
		})
	}
}

func TestDatabasePoolDiagnosticsCoalescesFlappingAndClampsResetDeltas(t *testing.T) {
	logs := &poolDiagnosticsLog{}
	logger := logf.New(logf.Opts{Writer: logs, Level: logf.InfoLevel})
	var state databasePoolDiagnosticsState
	start := time.Unix(1000, 0)
	sample := databasePoolSample{maxOpen: 2, open: 2, idle: 2, waitCount: 100, waitDuration: time.Hour}
	state.observe(start, sample, logger)
	for second := 1; second <= 120; second++ {
		sample.inUse, sample.idle = 0, 2
		if second%2 != 0 {
			sample.inUse, sample.idle = 2, 0
			sample.waitCount++
		}
		state.observe(start.Add(time.Duration(second)*time.Second), sample, logger)
	}
	lines := logs.snapshot()
	require.LessOrEqual(t, len(lines), 10, "sampled flapping must not produce one log per second")
	sample.inUse, sample.idle, sample.waitCount, sample.waitDuration = 0, 2, 0, 0
	state.observe(start.Add(3*time.Minute), sample, logger)
	lines = logs.snapshot()
	require.Contains(t, lines[len(lines)-1], "wait_count_delta=0")
	require.Contains(t, lines[len(lines)-1], "wait_duration_delta=0s")
}

// This uses a real, isolated PostgreSQL pool and the actual /ready handler.
// Only the clock ticks are controlled: Stats, pool wait counters, connection
// contention, Redis heartbeat and readiness ping are the production paths.
func TestDatabasePoolDiagnosticsPostgresSaturationBeforeReadinessFailure(t *testing.T) {
	db, pool := h1OpenPool(t, 2)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rdb.Set(ctx, queue.WorkerHeartbeatKey, time.Now().UTC().Format(time.RFC3339Nano), queue.WorkerHeartbeatTTL).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), queue.WorkerHeartbeatKey).Err() })
	logs := &poolDiagnosticsLog{events: make(chan string, 8)}
	app := &App{DB: db, Redis: rdb, Log: logf.New(logf.Opts{Writer: logs, Level: logf.InfoLevel})}
	app.realtimeSubscriberLive.Store(true)
	healthy := testutil.NewGETRequest(t)
	require.NoError(t, app.ReadyCheck(healthy))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(healthy))
	start := time.Now()
	ticks := make(chan time.Time, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		app.runDatabasePoolDiagnostics(ctx, start, ticks)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-monitorDone:
		case <-time.After(time.Second):
			t.Error("diagnostics loop did not join")
		}
	})
	awaitPoolDiagnostic(t, logs, "startup")
	baseline := pool.Stats()
	release := h1HoldConnections(t, pool, 2)
	defer release()
	workCtx, cancelWork := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWork()
	workDone := make(chan struct{})
	var workErr error
	go func() {
		defer close(workDone)
		workErr = pool.PingContext(workCtx)
	}()
	request := testutil.NewGETRequest(t)
	readyDone := make(chan struct{})
	var readyErr error
	go func() {
		defer close(readyDone)
		readyErr = app.ReadyCheck(request)
	}()
	t.Cleanup(func() {
		release()
		cancelWork()
		for _, done := range []<-chan struct{}{workDone, readyDone} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("pool waiter/readiness did not join after releasing connections")
			}
		}
	})
	require.Eventually(t, func() bool { return pool.Stats().WaitCount == baseline.WaitCount+2 }, time.Second, time.Millisecond)
	ticks <- start.Add(time.Second)
	saturated := awaitPoolDiagnostic(t, logs, "saturated")
	for _, field := range []string{"max_open=2", "open=2", "in_use=2", "idle=0", "wait_count_delta=2", "waits_in_sample=2", "saturated=true"} {
		require.Contains(t, saturated, field)
	}
	select {
	case <-readyDone:
		t.Fatalf("readiness completed before the saturation observation: %v", readyErr)
	default:
	}
	release()
	select {
	case <-workDone:
		require.NoError(t, workErr)
	case <-time.After(time.Second):
		t.Fatal("ordinary pool waiter did not recover")
	}
	select {
	case <-readyDone:
		require.NoError(t, readyErr)
	case <-time.After(time.Second):
		t.Fatal("readiness did not recover within its original ping budget")
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), "saturation was signalled without any failed readiness response")
	after := pool.Stats()
	require.Equal(t, baseline.WaitCount+2, after.WaitCount, "the observer must not borrow another connection")
	require.Greater(t, after.WaitDuration, baseline.WaitDuration)
	ticks <- start.Add(16 * time.Second)
	recovered := awaitPoolDiagnostic(t, logs, "recovered")
	require.Contains(t, recovered, "saturated=false")
	require.NotContains(t, recovered, "wait_duration_delta=0s")
	cancel()
	select {
	case <-monitorDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled observer did not return")
	}
	count := len(logs.snapshot())
	ticks <- start.Add(time.Hour)
	assert.Len(t, logs.snapshot(), count, "a joined monitor cannot emit later ticks")
	for _, line := range logs.snapshot() {
		assert.NotContains(t, line, "Database ping failed")
		assert.Less(t, len(line), 2048)
		assert.False(t, strings.Contains(line, "postgres://") || strings.Contains(line, "postgresql://"))
	}
}

func TestDatabasePoolDiagnosticsCancelledBeforeStartDoesNotLogOrRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil DB is deliberate: a cancelled observer must return before accessing
	// it, even when a tick is ready at the same time.
	logs := &poolDiagnosticsLog{}
	app := &App{Log: logf.New(logf.Opts{Writer: logs, Level: logf.InfoLevel})}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	app.runDatabasePoolDiagnostics(ctx, time.Now(), ticks)
	require.Empty(t, logs.snapshot())
}
