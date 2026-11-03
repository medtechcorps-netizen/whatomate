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

func metaDrainWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("drain fixture barrier timed out")
	}
}

func metaDrainResult(t *testing.T, ch <-chan metaRelayOutcome) metaRelayOutcome {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("drain owner did not return")
		return metaRelayOutcome{}
	}
}

func metaDrainGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

func TestMetaRelayJoinsWorkerAndActiveHTTPBeforeStoreClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerGate, releaseWorker := metaDrainGate(t)
	requestGate, releaseRequest := metaDrainGate(t)
	workerStarted, requestStarted := make(chan struct{}), make(chan struct{})
	httpReturned, shutdownStarted := make(chan struct{}), make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-requestGate
		w.WriteHeader(http.StatusNoContent)
	})}
	t.Cleanup(func() { _ = server.Close() })
	var closes atomic.Int32
	done := make(chan metaRelayOutcome, 1)
	go func() {
		done <- runMetaRelay(ctx, 5*time.Second, metaRelayRuntime{
			worker:   func(ctx context.Context) error { close(workerStarted); <-ctx.Done(); <-workerGate; return ctx.Err() },
			serve:    func() error { err := server.Serve(listener); close(httpReturned); return err },
			shutdown: func(ctx context.Context) error { close(shutdownStarted); return server.Shutdown(ctx) },
			stop:     func() {}, closeStore: func() error { closes.Add(1); return nil }, report: func(metaRelayOutcome) {},
		})
	}()
	metaDrainWait(t, workerStarted)
	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			err = response.Body.Close()
		}
		responseDone <- err
	}()
	metaDrainWait(t, requestStarted)
	cancel()
	metaDrainWait(t, shutdownStarted)
	metaDrainWait(t, httpReturned)
	if closes.Load() != 0 {
		t.Fatal("closed Redis before worker and active request returned")
	}
	releaseWorker()
	if closes.Load() != 0 {
		t.Fatal("listener return was mistaken for HTTP drain")
	}
	releaseRequest()
	result := metaDrainResult(t, done)
	if !result.complete || !result.reportJoined || result.failed || closes.Load() != 1 {
		t.Fatalf("drain=%+v closes=%d", result, closes.Load())
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
}

func TestMetaRelayUnexpectedExitDrainsPeerBeforeClose(t *testing.T) {
	for _, mode := range []string{"worker_error", "worker_nil", "http_error", "http_nil"} {
		t.Run(mode, func(t *testing.T) {
			httpStop := make(chan struct{})
			var closes, cancellations atomic.Int32
			result := runMetaRelay(context.Background(), 2*time.Second, metaRelayRuntime{
				worker: func(ctx context.Context) error {
					switch mode {
					case "worker_error":
						return errors.New("synthetic worker failure")
					case "worker_nil":
						return nil
					}
					<-ctx.Done()
					cancellations.Add(1)
					return ctx.Err()
				},
				serve: func() error {
					switch mode {
					case "http_error":
						return errors.New("synthetic listener failure")
					case "http_nil":
						return nil
					}
					<-httpStop
					return http.ErrServerClosed
				},
				shutdown: func(context.Context) error { close(httpStop); return nil }, stop: func() {},
				closeStore: func() error { closes.Add(1); return nil }, report: func(metaRelayOutcome) {},
			})
			if !result.complete || !result.reportJoined || !result.failed || closes.Load() != 1 {
				t.Fatalf("mode=%s result=%+v closes=%d", mode, result, closes.Load())
			}
			if (mode == "http_error" || mode == "http_nil") && cancellations.Load() != 1 {
				t.Fatal("worker was not cancelled and joined")
			}
		})
	}
}

func TestMetaRelayBlockedCallbackCannotExtendDrainBudget(t *testing.T) {
	for _, blocked := range []string{"worker", "shutdown", "stop", "store", "report"} {
		t.Run(blocked, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate, release := metaDrainGate(t)
			lateDone, httpStop := make(chan struct{}), make(chan struct{})
			workerStarted, httpStarted := make(chan struct{}), make(chan struct{})
			var closes atomic.Int32
			block := func(name string) {
				if blocked == name {
					<-gate
					close(lateDone)
				}
			}
			done := make(chan metaRelayOutcome, 1)
			go func() {
				done <- runMetaRelay(ctx, 200*time.Millisecond, metaRelayRuntime{
					worker:   func(ctx context.Context) error { close(workerStarted); <-ctx.Done(); block("worker"); return ctx.Err() },
					serve:    func() error { close(httpStarted); <-httpStop; return http.ErrServerClosed },
					shutdown: func(context.Context) error { close(httpStop); block("shutdown"); return nil },
					stop:     func() { block("stop") }, closeStore: func() error { closes.Add(1); block("store"); return nil },
					report: func(metaRelayOutcome) { block("report") },
				})
			}()
			metaDrainWait(t, workerStarted)
			metaDrainWait(t, httpStarted)
			start := time.Now()
			cancel()
			result := metaDrainResult(t, done)
			if time.Since(start) > 2*time.Second {
				t.Fatal("callback extended the owner budget")
			}
			if (blocked == "worker" || blocked == "shutdown") && closes.Load() != 0 {
				t.Fatal("store closed before users joined")
			}
			if blocked == "report" {
				if !result.complete || result.reportJoined {
					t.Fatalf("report result=%+v", result)
				}
			} else if result.complete {
				t.Fatalf("incomplete work claimed complete: %+v", result)
			}
			release()
			metaDrainWait(t, lateDone)
		})
	}
}

func TestMetaRelayCleanupFailuresRemainFailures(t *testing.T) {
	for _, failed := range []string{"shutdown", "close"} {
		t.Run(failed, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			httpStop := make(chan struct{})
			var closes atomic.Int32
			result := runMetaRelay(ctx, 100*time.Millisecond, metaRelayRuntime{
				worker: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
				serve:  func() error { <-httpStop; return http.ErrServerClosed },
				shutdown: func(context.Context) error {
					close(httpStop)
					if failed == "shutdown" {
						return errors.New("synthetic drain failure")
					}
					return nil
				},
				stop: func() {}, closeStore: func() error { closes.Add(1); return errors.New("synthetic close failure") }, report: func(metaRelayOutcome) {},
			})
			if result.complete || !result.failed {
				t.Fatalf("failure lost: %+v", result)
			}
			if failed == "shutdown" && closes.Load() != 0 {
				t.Fatal("store closed after unproved HTTP drain")
			}
		})
	}
}

type blockedMetaDrainLog struct{}

func (blockedMetaDrainLog) Write([]byte) (int, error) { select {} }

func TestMetaRelayProcessExitsWithBlockedCloseOrLog(t *testing.T) {
	if mode := os.Getenv("META_DRAIN_TEST_CHILD"); mode != "" {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		httpStop := make(chan struct{})
		logger := slog.New(slog.NewTextHandler(blockedMetaDrainLog{}, nil))
		result := runMetaRelay(ctx, 100*time.Millisecond, metaRelayRuntime{
			worker:   func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			serve:    func() error { <-httpStop; return http.ErrServerClosed },
			shutdown: func(context.Context) error { close(httpStop); return nil }, stop: func() {},
			closeStore: func() error {
				if mode == "close" {
					select {}
				}
				return nil
			},
			report: func(metaRelayOutcome) { logger.Info("static fixture report") },
		})
		if result.reportJoined || (mode == "close" && result.storeClosed) {
			os.Exit(71)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"close", "report"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMetaRelayProcessExitsWithBlockedCloseOrLog$")
			command.Env = append(os.Environ(), "META_DRAIN_TEST_CHILD="+mode)
			command.Stdout, command.Stderr = io.Discard, io.Discard
			start := time.Now()
			if err := command.Run(); err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("child exceeded shutdown allowance")
			}
		})
	}
}
