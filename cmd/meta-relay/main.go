package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shridarpatil/whatomate/internal/metarelay"
)

const metaRelayGracefulShutdownTimeout = 30 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	runtimeOwned, err := run(logger)
	if err != nil {
		// Keep this final boundary static. Individual components emit
		// allow-listed operational context, while arbitrary startup/runtime
		// errors can contain credentials, provider bodies, or connection URLs.
		if !runtimeOwned {
			logger.Error("Meta relay stopped", "reason", safeStartupReason(err))
		}
		os.Exit(1)
	}
}

func safeStartupReason(_ error) string {
	return "startup_or_runtime_failure"
}

func run(logger *slog.Logger) (runtimeOwned bool, err error) {
	config, err := metarelay.LoadConfig()
	if err != nil {
		return false, err
	}
	store, err := metarelay.NewRedisStore(config)
	if err != nil {
		return false, err
	}
	defer func() {
		if !runtimeOwned {
			_ = store.Close()
		}
	}()

	startupContext, startupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	pingErr := store.Ping(startupContext)
	startupCancel()
	if pingErr != nil {
		return false, errors.New("redis durability boundary is unavailable")
	}

	server, err := metarelay.NewServer(
		config,
		store,
		metarelay.WithServerLogger(logger),
	)
	if err != nil {
		return false, err
	}
	worker, err := metarelay.NewWorker(
		config,
		store,
		metarelay.WithWorkerLogger(logger),
	)
	if err != nil {
		return false, err
	}

	httpServer := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      metaRelayGracefulShutdownTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	// From this point the drain owner bounds cancellation, joins, store close
	// and reporting. No synchronous cleanup or log runs after it returns.
	rootContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	runtimeOwned = true
	outcome := runMetaRelay(rootContext, metaRelayGracefulShutdownTimeout, metaRelayRuntime{
		worker:     worker.Run,
		serve:      httpServer.ListenAndServe,
		shutdown:   httpServer.Shutdown,
		stop:       stop,
		closeStore: store.Close,
		report: func(result metaRelayOutcome) {
			logger.Info("Meta relay drain finished",
				"trigger", result.trigger,
				"complete", result.complete,
				"failed", result.failed,
				"worker_joined", result.workerJoined,
				"http_returned", result.httpReturned,
				"http_drained", result.httpDrained,
				"store_closed", result.storeClosed,
				"signal_stop_joined", result.stopJoined)
		},
	})
	if !outcome.complete || !outcome.reportJoined || outcome.failed {
		return true, errors.New("meta relay drain failed or incomplete")
	}
	return true, nil
}

// The command owns actual component returns, not just cancellation requests.
// No shared store is closed until the worker and graceful HTTP drain finish.
type metaRelayRuntime struct {
	worker     func(context.Context) error
	serve      func() error
	shutdown   func(context.Context) error
	stop       func()
	closeStore func() error
	report     func(metaRelayOutcome)
}

type metaRelayOutcome struct {
	trigger      string
	complete     bool
	failed       bool
	workerJoined bool
	httpReturned bool
	httpDrained  bool
	storeClosed  bool
	stopJoined   bool
	reportJoined bool
}

type metaRelayTask struct {
	done chan struct{}
	err  error
}

func startMetaRelayTask(updates chan<- struct{}, work func() error) *metaRelayTask {
	task := &metaRelayTask{done: make(chan struct{})}
	go func() {
		task.err = work()
		close(task.done) // publishes err and proves the callback returned
		select {
		case updates <- struct{}{}:
		default:
		}
	}()
	return task
}

func (task *metaRelayTask) result() (bool, error) {
	if task == nil {
		return false, nil
	}
	select {
	case <-task.done:
		return true, task.err
	default:
		return false, nil
	}
}

func runMetaRelay(signalContext context.Context, budget time.Duration, runtime metaRelayRuntime) metaRelayOutcome {
	// Signal cancellation is observed by the owner first. It establishes the
	// absolute deadline before requesting cancellation of worker intake.
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	updates := make(chan struct{}, 8)
	workerTask := startMetaRelayTask(updates, func() error { return runtime.worker(workerContext) })
	httpTask := startMetaRelayTask(updates, runtime.serve)
	result := metaRelayOutcome{}
	select {
	case <-signalContext.Done():
		result.trigger = "signal"
	case <-workerTask.done:
		result.trigger = "worker"
	case <-httpTask.done:
		result.trigger = "http"
	}
	// A loop/serve return before owned cancellation is unexpected even if nil.
	result.failed = result.trigger != "signal"
	drainContext, cancelDrain := context.WithTimeout(context.Background(), budget)
	defer cancelDrain()
	stopTask := startMetaRelayTask(updates, func() error {
		cancelWorkers()
		runtime.stop()
		return nil
	})
	shutdownTask := startMetaRelayTask(updates, func() error { return runtime.shutdown(drainContext) })
	var storeTask *metaRelayTask

	refresh := func() {
		var workerErr, httpErr, shutdownErr, storeErr error
		result.workerJoined, workerErr = workerTask.result()
		result.httpReturned, httpErr = httpTask.result()
		shutdownJoined, shutdownErr := shutdownTask.result()
		result.httpDrained = shutdownJoined && shutdownErr == nil
		result.stopJoined, _ = stopTask.result()
		storeJoined, storeErr := storeTask.result()
		result.storeClosed = storeJoined && storeErr == nil
		result.failed = result.failed ||
			(workerErr != nil && !errors.Is(workerErr, context.Canceled)) ||
			(httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed)) ||
			shutdownErr != nil || storeErr != nil
		result.complete = result.workerJoined &&
			result.httpReturned && result.httpDrained && result.stopJoined &&
			result.storeClosed
	}
	for {
		refresh()
		if drainContext.Err() != nil {
			break
		}
		if storeTask == nil && result.workerJoined && result.httpReturned && result.httpDrained {
			// The store remains available until all work that can use it has
			// returned. Its close shares the original remaining drain budget.
			storeTask = startMetaRelayTask(updates, runtime.closeStore)
			continue
		}
		storeJoined, _ := storeTask.result()
		if storeJoined && result.stopJoined {
			break
		}
		select {
		case <-updates:
		case <-drainContext.Done():
		}
	}
	refresh()
	// Reporting is itself owned and bounded. main must not log or close again
	// after this return. At expiry an incomplete report may never be emitted.
	reportDone := make(chan struct{})
	go func(snapshot metaRelayOutcome) {
		runtime.report(snapshot)
		close(reportDone)
	}(result)
	select {
	case <-reportDone:
		result.reportJoined = true
	case <-drainContext.Done():
	}
	return result
}
