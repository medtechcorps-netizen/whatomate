package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownWorkerFixture struct {
	loops [4]func(context.Context) error
	close func() error
}

func (w *shutdownWorkerFixture) Run(ctx context.Context) error              { return w.loops[0](ctx) }
func (w *shutdownWorkerFixture) RunChannelOutbox(ctx context.Context) error { return w.loops[1](ctx) }
func (w *shutdownWorkerFixture) RunChannelAIReplies(ctx context.Context) error {
	return w.loops[2](ctx)
}
func (w *shutdownWorkerFixture) RunThreadsCredentialRefresh(ctx context.Context) error {
	return w.loops[3](ctx)
}
func (w *shutdownWorkerFixture) Close() error {
	if w.close != nil {
		return w.close()
	}
	return nil
}

func TestStandaloneWorkersJoinEveryLoopBeforeClose(t *testing.T) {
	started := make(chan struct{}, 4)
	settle := make(chan struct{})
	defer func() {
		select {
		case <-settle:
		default:
			close(settle)
		}
	}()
	var returned atomic.Int32
	var closed atomic.Bool
	w := &shutdownWorkerFixture{close: func() error {
		if returned.Load() != 4 {
			return errors.New("closed before every loop settled")
		}
		closed.Store(true)
		return nil
	}}
	for i := range w.loops {
		w.loops[i] = func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			<-settle
			returned.Add(1)
			return ctx.Err()
		}
	}
	quit := make(chan os.Signal, 1)
	done := make(chan serverShutdownResult, 1)
	go func() { done <- runStandaloneWorkers([]standaloneWorker{w}, quit, time.Second, nil) }()
	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("loop did not start")
		}
	}
	quit <- os.Interrupt
	select {
	case <-done:
		t.Fatal("stop request was misreported as a join")
	case <-time.After(20 * time.Millisecond):
	}
	if closed.Load() {
		t.Fatal("Close ran while loops were settling")
	}
	close(settle)
	result := awaitShutdownResult(t, done)
	if result.deadlineExpired || len(result.failed) > 0 || len(result.pending) > 0 || len(result.completed) != 6 || !closed.Load() {
		t.Fatalf("unexpected drain: %+v", result)
	}
}

func TestStandaloneWorkerEarlyLoopFailureCancelsSiblings(t *testing.T) {
	want := errors.New("synthetic loop failure")
	w := &shutdownWorkerFixture{}
	w.loops[0] = func(context.Context) error { return want }
	var cancelled atomic.Int32
	for i := 1; i < 4; i++ {
		w.loops[i] = func(ctx context.Context) error { <-ctx.Done(); cancelled.Add(1); return ctx.Err() }
	}
	result := runStandaloneWorkers([]standaloneWorker{w}, nil, time.Second, nil)
	if cancelled.Load() != 3 || result.failed["worker 1 message loop"] != want || result.deadlineExpired {
		t.Fatalf("failure did not drain siblings: %+v cancelled=%d", result, cancelled.Load())
	}
}

func TestStandaloneWorkerDeadlineBoundsUnjoinedLoopCloseAndReporter(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	blocked := &shutdownWorkerFixture{}
	for i := range blocked.loops {
		blocked.loops[i] = func(context.Context) error { <-release; return nil }
	}
	closer := &shutdownWorkerFixture{close: func() error { <-release; return nil }}
	for i := range closer.loops {
		closer.loops[i] = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	}
	quit := make(chan os.Signal, 1)
	quit <- os.Interrupt
	start := time.Now()
	result := runStandaloneWorkers([]standaloneWorker{blocked, closer}, quit, 75*time.Millisecond, func(serverShutdownResult) { <-release })
	if !result.deadlineExpired || time.Since(start) > time.Second {
		t.Fatalf("independent deadline not honored: %+v", result)
	}
	if len(result.pending) == 0 {
		t.Fatalf("unjoined work reported complete: %+v", result)
	}
}

func TestStandaloneWorkerNormalLoopExitAlsoDrains(t *testing.T) {
	w := &shutdownWorkerFixture{}
	w.loops[0] = func(context.Context) error { return nil }
	for i := 1; i < 4; i++ {
		w.loops[i] = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	}
	result := runStandaloneWorkers([]standaloneWorker{w}, nil, time.Second, nil)
	if result.deadlineExpired || len(result.failed) > 0 || len(result.completed) != 6 {
		t.Fatalf("normal early exit did not drain: %+v", result)
	}
}

func TestStandaloneWorkerShutdownProcessExit(t *testing.T) {
	const childFlag = "WHATOMATE_WORKER_SHUTDOWN_TEST_CHILD"
	if os.Getenv(childFlag) == "1" {
		never := make(chan struct{})
		w := &shutdownWorkerFixture{close: func() error { <-never; return nil }}
		for i := range w.loops {
			w.loops[i] = func(context.Context) error { <-never; return nil }
		}
		quit := make(chan os.Signal, 1)
		quit <- os.Interrupt
		result := runStandaloneWorkers([]standaloneWorker{w}, quit, 25*time.Millisecond, func(serverShutdownResult) { <-never })
		if !result.deadlineExpired {
			t.Fatal("deadline not reached")
		}
		return // Normal test-process return with unjoined work, not os.Exit.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandaloneWorkerShutdownProcessExit$", "-test.timeout=2s")
	cmd.Env = append(os.Environ(), childFlag+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker shutdown prevented process exit: %v\n%s", err, output)
	}
}
