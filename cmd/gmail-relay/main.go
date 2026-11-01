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

	"github.com/shridarpatil/whatomate/internal/gmailrelay"
	"github.com/shridarpatil/whatomate/internal/metarelay"
)

const gracefulShutdownTimeout = 30 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	runtimeOwned, err := run(logger)
	if err != nil {
		if !runtimeOwned {
			logger.Error("Gmail relay stopped", "reason", "startup_failure")
		}
		os.Exit(1)
	}
}

func run(logger *slog.Logger) (runtimeOwned bool, err error) {
	config, err := gmailrelay.LoadConfig()
	if err != nil {
		return false, err
	}
	stateStore, err := gmailrelay.NewRedisStore(config)
	if err != nil {
		return false, err
	}
	defer func() {
		if !runtimeOwned {
			_ = stateStore.Close()
		}
	}()

	queueStore, err := metarelay.NewRedisStore(&metarelay.Config{
		RedisURL:          config.RedisURL,
		RedisPrefix:       config.RedisPrefix + "queue:",
		InboundRetention:  config.InboundRetention,
		OutboundRetention: config.OutboundRetention,
	})
	if err != nil {
		return false, err
	}
	defer func() {
		if !runtimeOwned {
			_ = queueStore.Close()
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStartup()
	if err := stateStore.Ping(startupCtx); err != nil {
		return false, errors.New("gmail OAuth durability boundary is unavailable")
	}
	if err := queueStore.Ping(startupCtx); err != nil {
		return false, errors.New("gmail message durability boundary is unavailable")
	}

	oauth, err := gmailrelay.NewOAuthService(config, stateStore, nil)
	if err != nil {
		return false, err
	}
	gmail, err := gmailrelay.NewGmailClient(config, oauth, nil)
	if err != nil {
		return false, err
	}
	server, err := gmailrelay.NewServer(
		config,
		stateStore,
		oauth,
		gmail,
		queueStore,
		gmailrelay.WithServerLogger(logger),
	)
	if err != nil {
		return false, err
	}
	syncer, err := gmailrelay.NewSyncer(
		config,
		gmail,
		stateStore,
		queueStore,
		gmailrelay.WithSyncerLogger(logger),
	)
	if err != nil {
		return false, err
	}
	forwarder, err := gmailrelay.NewForwardWorker(
		config,
		queueStore,
		gmailrelay.WithForwardWorkerLogger(logger),
	)
	if err != nil {
		return false, err
	}

	httpServer := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      gracefulShutdownTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	// Constructors finish before signals are intercepted. Once the owner starts,
	// it alone owns stop requests, joins, runtime cleanup and bounded reporting.
	rootContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	runtimeOwned = true
	outcome := runGmailRelay(rootContext, gracefulShutdownTimeout, gmailRelayRuntime{
		sync:       syncer.Run,
		forward:    forwarder.Run,
		serve:      httpServer.ListenAndServe,
		shutdown:   httpServer.Shutdown,
		stop:       stop,
		closeState: stateStore.Close,
		closeQueue: queueStore.Close,
		report: func(result gmailRelayOutcome) {
			logger.Info("Gmail relay drain finished",
				"trigger", result.trigger,
				"complete", result.complete,
				"failed", result.failed,
				"sync_joined", result.syncJoined,
				"forward_joined", result.forwardJoined,
				"http_returned", result.httpReturned,
				"http_drained", result.httpDrained,
				"state_closed", result.stateClosed,
				"queue_closed", result.queueClosed,
				"signal_stop_joined", result.stopJoined)
		},
	})
	if !outcome.complete || !outcome.reportJoined || outcome.failed {
		return true, errors.New("gmail relay drain failed or incomplete")
	}
	return true, nil
}

// The command owns actual component returns, not just cancellation requests.
// No shared store is closed until both loops and graceful HTTP drain finish.
type gmailRelayRuntime struct {
	sync       func(context.Context) error
	forward    func(context.Context) error
	serve      func() error
	shutdown   func(context.Context) error
	stop       func()
	closeState func() error
	closeQueue func() error
	report     func(gmailRelayOutcome)
}

type gmailRelayOutcome struct {
	trigger       string
	complete      bool
	failed        bool
	syncJoined    bool
	forwardJoined bool
	httpReturned  bool
	httpDrained   bool
	stateClosed   bool
	queueClosed   bool
	stopJoined    bool
	reportJoined  bool
}

type gmailRelayTask struct {
	done chan struct{}
	err  error
}

func startGmailRelayTask(updates chan<- struct{}, work func() error) *gmailRelayTask {
	task := &gmailRelayTask{done: make(chan struct{})}
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

func (task *gmailRelayTask) result() (bool, error) {
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

func runGmailRelay(signalContext context.Context, budget time.Duration, runtime gmailRelayRuntime) gmailRelayOutcome {
	// Signal cancellation is observed by the owner first. It establishes the
	// absolute deadline before requesting cancellation of worker intake.
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	updates := make(chan struct{}, 8)
	syncTask := startGmailRelayTask(updates, func() error { return runtime.sync(workerContext) })
	forwardTask := startGmailRelayTask(updates, func() error { return runtime.forward(workerContext) })
	httpTask := startGmailRelayTask(updates, runtime.serve)
	result := gmailRelayOutcome{}
	select {
	case <-signalContext.Done():
		result.trigger = "signal"
	case <-syncTask.done:
		result.trigger = "sync"
	case <-forwardTask.done:
		result.trigger = "forward"
	case <-httpTask.done:
		result.trigger = "http"
	}
	// A loop/serve return before owned cancellation is unexpected even if nil.
	result.failed = result.trigger != "signal"
	drainContext, cancelDrain := context.WithTimeout(context.Background(), budget)
	defer cancelDrain()
	stopTask := startGmailRelayTask(updates, func() error {
		cancelWorkers()
		runtime.stop()
		return nil
	})
	shutdownTask := startGmailRelayTask(updates, func() error { return runtime.shutdown(drainContext) })
	var stateTask, queueTask *gmailRelayTask

	refresh := func() {
		var syncErr, forwardErr, httpErr, shutdownErr, stateErr, queueErr error
		result.syncJoined, syncErr = syncTask.result()
		result.forwardJoined, forwardErr = forwardTask.result()
		result.httpReturned, httpErr = httpTask.result()
		shutdownJoined, shutdownErr := shutdownTask.result()
		result.httpDrained = shutdownJoined && shutdownErr == nil
		result.stopJoined, _ = stopTask.result()
		stateJoined, stateErr := stateTask.result()
		queueJoined, queueErr := queueTask.result()
		result.stateClosed = stateJoined && stateErr == nil
		result.queueClosed = queueJoined && queueErr == nil
		result.failed = result.failed ||
			(syncErr != nil && !errors.Is(syncErr, context.Canceled)) ||
			(forwardErr != nil && !errors.Is(forwardErr, context.Canceled)) ||
			(httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed)) ||
			shutdownErr != nil || stateErr != nil || queueErr != nil
		result.complete = result.syncJoined && result.forwardJoined &&
			result.httpReturned && result.httpDrained && result.stopJoined &&
			result.stateClosed && result.queueClosed
	}
	for {
		refresh()
		if drainContext.Err() != nil {
			break
		}
		if stateTask == nil && result.syncJoined && result.forwardJoined && result.httpReturned && result.httpDrained {
			// Independent closes share the same remaining budget; one blocked
			// closer cannot suppress its sibling or force premature disposal.
			stateTask = startGmailRelayTask(updates, runtime.closeState)
			queueTask = startGmailRelayTask(updates, runtime.closeQueue)
			continue
		}
		stateJoined, _ := stateTask.result()
		queueJoined, _ := queueTask.result()
		if stateJoined && queueJoined && result.stopJoined {
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
	go func(snapshot gmailRelayOutcome) {
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
