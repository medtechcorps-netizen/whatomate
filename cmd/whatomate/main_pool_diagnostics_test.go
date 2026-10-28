package main

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
)

type blockedPoolDiagnosticWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedPoolDiagnosticWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func requireDiagnosticPending(t *testing.T, result serverShutdownResult) {
	t.Helper()
	require.NotContains(t, result.completed, "database pool diagnostics", "cancellation is not a completed join")
	_, failed := result.failed["database pool diagnostics"]
	if !failed {
		require.Contains(t, result.pending, "database pool diagnostics")
	}
}

func TestDatabasePoolDiagnosticsServerOwnerBoundsBlockedLogSink(t *testing.T) {
	db := testutil.SetupTestDB(t)
	writer := &blockedPoolDiagnosticWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(writer.release) }) })
	app := &handlers.App{DB: db, Log: logf.New(logf.Opts{Writer: writer, Level: logf.InfoLevel})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop := startServerLoop("database pool diagnostics", func() { app.RunDatabasePoolDiagnostics(ctx) })
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(writer.release) })
		joined, stopJoin := context.WithTimeout(context.Background(), time.Second)
		defer stopJoin()
		if err := loop.run(joined); err != nil {
			t.Errorf("diagnostics cleanup did not join: %v", err)
		}
	})
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("production diagnostic did not enter its synchronous writer")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	result := shutdownServer(deadline, []serverShutdownTask{
		{"database pool diagnostics stop request", func(context.Context) error { cancel(); return nil }},
		loop,
	}, nil)
	require.Less(t, time.Since(deadline), time.Second)
	requireDiagnosticPending(t, result)
	// The writer really stayed blocked after command shutdown. Only releasing
	// it lets the loop observe cancellation, proving no fictitious log timeout.
	release.Do(func() { close(writer.release) })
	joined, stopJoin := context.WithTimeout(context.Background(), time.Second)
	defer stopJoin()
	require.NoError(t, loop.run(joined))
}

func TestDatabasePoolDiagnosticsStandaloneOwnerBoundsBlockedLogSink(t *testing.T) {
	db := testutil.SetupTestDB(t)
	writer := &blockedPoolDiagnosticWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(writer.release) }) })
	app := &handlers.App{DB: db, Log: logf.New(logf.Opts{Writer: writer, Level: logf.InfoLevel})}
	quit := make(chan os.Signal, 1)
	done := make(chan serverShutdownResult, 1)
	joined := make(chan struct{})
	ownerDone := make(chan struct{})
	t.Cleanup(func() {
		release.Do(func() { close(writer.release) })
		stopPoolDiagnosticTestOwner(t, quit, ownerDone)
	})
	go func() {
		defer close(ownerDone)
		done <- runStandaloneWorkers(nil, quit, 100*time.Millisecond, nil, standaloneBackgroundLoop{
			"database pool diagnostics", func(ctx context.Context) {
				defer close(joined)
				app.RunDatabasePoolDiagnostics(ctx)
			},
		})
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("production diagnostic did not enter its synchronous writer")
	}
	quit <- syscall.SIGTERM
	result := awaitShutdownResult(t, done)
	requireDiagnosticPending(t, result)
	select {
	case <-joined:
		t.Fatal("blocked logger incorrectly counted as joined")
	default:
	}
	release.Do(func() { close(writer.release) })
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("diagnostic did not join after writer release")
	}
}

func TestDatabasePoolDiagnosticsStandaloneCompletionCannotStopWorkers(t *testing.T) {
	quit := make(chan os.Signal, 1)
	returned := make(chan struct{})
	done := make(chan serverShutdownResult, 1)
	ownerDone := make(chan struct{})
	t.Cleanup(func() { stopPoolDiagnosticTestOwner(t, quit, ownerDone) })
	go func() {
		defer close(ownerDone)
		done <- runStandaloneWorkers(nil, quit, time.Second, nil, standaloneBackgroundLoop{
			"database pool diagnostics", func(context.Context) { close(returned) },
		})
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("observational loop did not run")
	}
	select {
	case result := <-done:
		t.Fatalf("diagnostic completion became an exit watchdog: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	quit <- syscall.SIGTERM
	result := awaitShutdownResult(t, done)
	require.False(t, result.deadlineExpired)
	require.Empty(t, result.failed)
	require.Empty(t, result.pending)
	require.Contains(t, result.completed, "database pool diagnostics")
}

func TestDatabasePoolDiagnosticsStandaloneCancellationJoins(t *testing.T) {
	quit := make(chan os.Signal, 1)
	started := make(chan struct{})
	joined := make(chan struct{})
	done := make(chan serverShutdownResult, 1)
	ownerDone := make(chan struct{})
	t.Cleanup(func() { stopPoolDiagnosticTestOwner(t, quit, ownerDone) })
	go func() {
		defer close(ownerDone)
		done <- runStandaloneWorkers(nil, quit, time.Second, nil, standaloneBackgroundLoop{
			"database pool diagnostics", func(ctx context.Context) {
				close(started)
				<-ctx.Done()
				close(joined)
			},
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("owned loop did not start")
	}
	quit <- syscall.SIGTERM
	result := awaitShutdownResult(t, done)
	require.Empty(t, result.pending)
	require.Empty(t, result.failed)
	require.False(t, result.deadlineExpired)
	require.Contains(t, result.completed, "database pool diagnostics")
	select {
	case <-joined:
	default:
		t.Fatal("owner reported completion without the loop returning")
	}
}

func stopPoolDiagnosticTestOwner(t *testing.T, quit chan<- os.Signal, done <-chan struct{}) {
	t.Helper()
	select {
	case quit <- syscall.SIGTERM:
	default:
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("diagnostic test owner did not return during cleanup")
	}
}
