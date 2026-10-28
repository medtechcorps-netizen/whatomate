package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitRelaySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for relay fixture barrier")
	}
}

func receiveRelayOutcome(t *testing.T, ch <-chan gmailRelayOutcome) gmailRelayOutcome {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("relay owner did not return")
		return gmailRelayOutcome{}
	}
}

func relayGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

func TestGmailRelayJoinsLoopsAndHTTPBeforeStores(t *testing.T) {
	root, signalStop := context.WithCancel(context.Background())
	defer signalStop()
	syncRelease, releaseSync := relayGate(t)
	forwardRelease, releaseForward := relayGate(t)
	requestRelease, releaseRequest := relayGate(t)
	syncStarted, forwardStarted, requestStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	httpReturned, shutdownStarted := make(chan struct{}), make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-requestRelease
		w.WriteHeader(http.StatusNoContent)
	})}
	t.Cleanup(func() { _ = server.Close() })
	var closes atomic.Int32
	done := make(chan gmailRelayOutcome, 1)
	go func() {
		done <- runGmailRelay(root, 5*time.Second, gmailRelayRuntime{
			sync: func(ctx context.Context) error { close(syncStarted); <-ctx.Done(); <-syncRelease; return ctx.Err() },
			forward: func(ctx context.Context) error {
				close(forwardStarted)
				<-ctx.Done()
				<-forwardRelease
				return ctx.Err()
			},
			serve:      func() error { err := server.Serve(listener); close(httpReturned); return err },
			shutdown:   func(ctx context.Context) error { close(shutdownStarted); return server.Shutdown(ctx) },
			stop:       func() {},
			closeState: func() error { closes.Add(1); return nil },
			closeQueue: func() error { closes.Add(1); return nil },
			report:     func(gmailRelayOutcome) {},
		})
	}()
	waitRelaySignal(t, syncStarted)
	waitRelaySignal(t, forwardStarted)
	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			err = response.Body.Close()
		}
		responseDone <- err
	}()
	waitRelaySignal(t, requestStarted)
	signalStop()
	waitRelaySignal(t, shutdownStarted)
	waitRelaySignal(t, httpReturned)
	if closes.Load() != 0 {
		t.Fatal("closed stores while active HTTP request and workers remained")
	}
	releaseSync()
	releaseForward()
	if closes.Load() != 0 {
		t.Fatal("ListenAndServe return was mistaken for HTTP drain")
	}
	releaseRequest()
	result := receiveRelayOutcome(t, done)
	if !result.complete || !result.reportJoined || result.failed || closes.Load() != 2 {
		t.Fatalf("drain = %+v, close count = %d", result, closes.Load())
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
}

func TestGmailRelayAnyComponentExitDrainsPeers(t *testing.T) {
	for _, component := range []string{"sync", "forward", "http", "sync_nil"} {
		t.Run(component, func(t *testing.T) {
			httpStop := make(chan struct{})
			var closes, cancelled atomic.Int32
			loop := func(name string) func(context.Context) error {
				return func(ctx context.Context) error {
					if component == name {
						return errors.New("synthetic component failure")
					}
					if component == name+"_nil" {
						return nil
					}
					<-ctx.Done()
					cancelled.Add(1)
					return ctx.Err()
				}
			}
			result := runGmailRelay(context.Background(), 2*time.Second, gmailRelayRuntime{
				sync: loop("sync"), forward: loop("forward"),
				serve: func() error {
					if component == "http" {
						return errors.New("synthetic listen failure")
					}
					<-httpStop
					return http.ErrServerClosed
				},
				shutdown:   func(context.Context) error { close(httpStop); return nil },
				stop:       func() {},
				closeState: func() error { closes.Add(1); return nil },
				closeQueue: func() error { closes.Add(1); return nil },
				report:     func(gmailRelayOutcome) {},
			})
			if !result.complete || !result.reportJoined || !result.failed || closes.Load() != 2 {
				t.Fatalf("component %s outcome = %+v, closes=%d", component, result, closes.Load())
			}
			expectedCancelled := int32(1)
			if component == "http" {
				expectedCancelled = 2
			}
			if cancelled.Load() != expectedCancelled {
				t.Fatalf("peer cancellation count=%d", cancelled.Load())
			}
		})
	}
}

func TestGmailRelayIndependentDeadlineAndLateCompletion(t *testing.T) {
	for _, blocked := range []string{"stop", "sync", "shutdown", "state_close", "report"} {
		t.Run(blocked, func(t *testing.T) {
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate, release := relayGate(t)
			lateDone := make(chan struct{})
			httpStop := make(chan struct{})
			syncStarted, forwardStarted, httpStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			stopStarted, shutdownStarted := make(chan struct{}), make(chan struct{})
			var queueCloses, stateCloses atomic.Int32
			block := func() { <-gate; close(lateDone) }
			done := make(chan gmailRelayOutcome, 1)
			go func() {
				done <- runGmailRelay(root, 200*time.Millisecond, gmailRelayRuntime{
					sync: func(ctx context.Context) error {
						close(syncStarted)
						<-ctx.Done()
						if blocked == "sync" {
							block()
						}
						return ctx.Err()
					},
					forward: func(ctx context.Context) error { close(forwardStarted); <-ctx.Done(); return ctx.Err() },
					serve:   func() error { close(httpStarted); <-httpStop; return http.ErrServerClosed },
					shutdown: func(context.Context) error {
						close(shutdownStarted)
						close(httpStop)
						if blocked == "shutdown" {
							block()
						}
						return nil
					},
					stop: func() {
						close(stopStarted)
						if blocked == "stop" {
							block()
						}
					},
					closeState: func() error {
						stateCloses.Add(1)
						if blocked == "state_close" {
							block()
						}
						return nil
					},
					closeQueue: func() error { queueCloses.Add(1); return nil },
					report: func(gmailRelayOutcome) {
						if blocked == "report" {
							block()
						}
					},
				})
			}()
			waitRelaySignal(t, syncStarted)
			waitRelaySignal(t, forwardStarted)
			waitRelaySignal(t, httpStarted)
			start := time.Now()
			cancel()
			waitRelaySignal(t, stopStarted)
			waitRelaySignal(t, shutdownStarted)
			result := receiveRelayOutcome(t, done)
			if time.Since(start) > 2*time.Second {
				t.Fatal("callback extended the absolute owner budget")
			}
			if blocked == "sync" || blocked == "shutdown" {
				if stateCloses.Load() != 0 || queueCloses.Load() != 0 {
					t.Fatal("closed shared stores before full join")
				}
			}
			if blocked == "state_close" && queueCloses.Load() != 1 {
				t.Fatal("blocked close suppressed sibling close")
			}
			if blocked == "report" {
				if !result.complete || result.reportJoined {
					t.Fatalf("report outcome = %+v", result)
				}
			} else if result.complete {
				t.Fatalf("incomplete work claimed complete: %+v", result)
			}
			release()
			waitRelaySignal(t, lateDone)
		})
	}
}

type blockedRelayLogWriter struct{}

func (blockedRelayLogWriter) Write([]byte) (int, error) { select {} }

func TestGmailRelayProcessExitsWithinOwnedBudget(t *testing.T) {
	// Only the test binary reads this fixture flag. The real command has no
	// test-only environment branch or relaxed runtime configuration.
	if mode := os.Getenv("GMAIL_DRAIN_TEST_CHILD"); mode != "" {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stopHTTP := make(chan struct{})
		logger := slog.New(slog.NewTextHandler(blockedRelayLogWriter{}, nil))
		result := runGmailRelay(ctx, 100*time.Millisecond, gmailRelayRuntime{
			sync:     func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			forward:  func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			serve:    func() error { <-stopHTTP; return http.ErrServerClosed },
			shutdown: func(context.Context) error { close(stopHTTP); return nil },
			stop:     func() {},
			closeState: func() error {
				if mode == "close" {
					select {}
				}
				return nil
			},
			closeQueue: func() error { return nil },
			report:     func(gmailRelayOutcome) { logger.Info("static fixture report") },
		})
		if result.reportJoined || (mode == "close" && result.stateClosed) {
			os.Exit(71)
		}
		// Like main's owned-runtime path, exit without another log or cleanup.
		os.Exit(0)
	}
	for _, mode := range []string{"close", "report"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGmailRelayProcessExitsWithinOwnedBudget$")
			command.Env = append(os.Environ(), "GMAIL_DRAIN_TEST_CHILD="+mode)
			command.Stdout, command.Stderr = io.Discard, io.Discard
			start := time.Now()
			if err := command.Run(); err != nil {
				t.Fatalf("child outcome: %v", err)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("child exceeded bounded shutdown allowance")
			}
		})
	}
}
