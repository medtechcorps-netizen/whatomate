package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func awaitShutdownResult(t *testing.T, done <-chan serverShutdownResult) serverShutdownResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(time.Second):
		t.Fatal("shutdown owner did not return")
		return serverShutdownResult{}
	}
}

func TestServerShutdownBoundsBlockedCallbacksAndLogger(t *testing.T) {
	if serverShutdownBudget != 25*time.Second {
		t.Fatalf("unexpected whole-server budget: %v", serverShutdownBudget)
	}
	release := make(chan struct{})
	releaseLog := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	defer close(releaseLog)
	started := make(chan string, 3)
	returned := make(chan string, 3)
	tasks := make([]serverShutdownTask, 0, 4)
	for _, name := range []string{"HTTP shutdown", "subscriber close", "worker close"} {
		tasks = append(tasks, serverShutdownTask{name, func(context.Context) error {
			started <- name
			<-release
			returned <- name
			return nil
		}})
	}
	cancelled := make(chan struct{})
	tasks = append(tasks, serverShutdownTask{"independent stop request", func(context.Context) error {
		close(cancelled)
		return nil
	}})
	logStarted := make(chan struct{})
	done := make(chan serverShutdownResult, 1)
	deadline := time.Now().Add(100 * time.Millisecond)
	go func() {
		done <- shutdownServer(deadline, tasks, func(serverShutdownResult) {
			close(logStarted)
			<-releaseLog
		})
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("a blocked callback prevented another shutdown attempt")
		}
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("blocked closes prevented an independent cancellation")
	}
	result := awaitShutdownResult(t, done)
	if !result.deadlineExpired || len(result.pending) != 3 {
		t.Fatalf("blocked callbacks were not reported pending: %+v", result)
	}
	if time.Now().Before(deadline) {
		t.Fatal("owner returned before its deadline despite blocked tasks")
	}
	select {
	case <-logStarted:
	case <-time.After(time.Second):
		t.Fatal("best-effort reporter was not invoked")
	}
	// Unblock each task after the owner returned. Its result has a reserved
	// buffer slot, so late completion cannot depend on the owner receiving it.
	close(release)
	for range 3 {
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("late callback did not return")
		}
	}
}

func TestServerShutdownStopRequestDoesNotProveLoopJoined(t *testing.T) {
	stopRequested := make(chan struct{})
	loopSawStop := make(chan struct{})
	releaseLoop := make(chan struct{})
	defer func() {
		select {
		case <-releaseLoop:
		default:
			close(releaseLoop)
		}
	}()
	loop := startServerLoop("owned loop", func() {
		<-stopRequested
		close(loopSawStop)
		<-releaseLoop
	})
	tasks := []serverShutdownTask{
		{"signal-only stop", func(context.Context) error {
			close(stopRequested)
			return nil
		}},
		{"no-op close", func(context.Context) error { return nil }},
		loop,
	}
	done := make(chan serverShutdownResult, 1)
	go func() {
		done <- shutdownServer(time.Now().Add(time.Second), tasks, nil)
	}()
	select {
	case <-loopSawStop:
	case <-time.After(time.Second):
		t.Fatal("stop was not requested")
	}
	select {
	case result := <-done:
		t.Fatalf("stop/no-op Close was mistaken for actual completion: %+v", result)
	default:
	}
	close(releaseLoop)
	result := awaitShutdownResult(t, done)
	if result.deadlineExpired || len(result.failed) != 0 || len(result.pending) != 0 || len(result.completed) != 3 {
		t.Fatalf("cooperative completion was not observed: %+v", result)
	}
}

func TestServerShutdownKeepsExistingAbsoluteDeadline(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	seen := make(chan time.Time, 1)
	done := make(chan serverShutdownResult, 1)
	go func() {
		done <- shutdownServer(deadline, []serverShutdownTask{{"deadline witness", func(ctx context.Context) error {
			actual, ok := ctx.Deadline()
			if !ok {
				actual = time.Time{}
			}
			seen <- actual
			return ctx.Err()
		}}}, nil)
	}()
	result := awaitShutdownResult(t, done)
	if !result.deadlineExpired {
		t.Fatalf("expired deadline was renewed: %+v", result)
	}
	select {
	case actual := <-seen:
		if actual != deadline {
			t.Fatalf("callback deadline = %v, want %v", actual, deadline)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not attempted")
	}
}

func TestServerShutdownRetainsCallbackFailure(t *testing.T) {
	want := errors.New("synthetic close failure")
	result := shutdownServer(time.Now().Add(time.Second), []serverShutdownTask{
		{"failed close", func(context.Context) error { return want }},
	}, nil)
	if result.deadlineExpired || len(result.pending) != 0 || len(result.completed) != 0 || len(result.failed) != 1 || result.failed["failed close"] != want {
		t.Fatalf("callback failure was lost or reported as completion: %+v", result)
	}
}

func TestServerShutdownProcessExit(t *testing.T) {
	const childFlag = "WHATOMATE_SHUTDOWN_TEST_CHILD"
	if os.Getenv(childFlag) == "1" {
		never := make(chan struct{})
		result := shutdownServer(time.Now().Add(25*time.Millisecond), []serverShutdownTask{
			{"uncooperative close", func(context.Context) error { <-never; return nil }},
		}, func(serverShutdownResult) { <-never })
		if !result.deadlineExpired {
			t.Fatal("child did not reach its shutdown deadline")
		}
		// The test process returns normally with both goroutines still blocked,
		// just as main returns immediately after runServer. No os.Exit shortcut.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerShutdownProcessExit$", "-test.timeout=2s")
	cmd.Env = append(os.Environ(), childFlag+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("normal process return was held by shutdown work: %v\n%s", err, output)
	}
}
