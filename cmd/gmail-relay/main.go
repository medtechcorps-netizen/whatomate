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
	if err := run(logger); err != nil {
		logger.Error("Gmail relay stopped", "reason", "startup_or_runtime_failure")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := gmailrelay.LoadConfig()
	if err != nil {
		return err
	}
	stateStore, err := gmailrelay.NewRedisStore(config)
	if err != nil {
		return err
	}
	defer func() { _ = stateStore.Close() }()

	queueStore, err := metarelay.NewRedisStore(&metarelay.Config{
		RedisURL:          config.RedisURL,
		RedisPrefix:       config.RedisPrefix + "queue:",
		InboundRetention:  config.InboundRetention,
		OutboundRetention: config.OutboundRetention,
	})
	if err != nil {
		return err
	}
	defer func() { _ = queueStore.Close() }()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStartup()
	if err := stateStore.Ping(startupCtx); err != nil {
		return errors.New("gmail OAuth durability boundary is unavailable")
	}
	if err := queueStore.Ping(startupCtx); err != nil {
		return errors.New("gmail message durability boundary is unavailable")
	}

	oauth, err := gmailrelay.NewOAuthService(config, stateStore, nil)
	if err != nil {
		return err
	}
	gmail, err := gmailrelay.NewGmailClient(config, oauth, nil)
	if err != nil {
		return err
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
		return err
	}
	syncer, err := gmailrelay.NewSyncer(
		config,
		gmail,
		stateStore,
		queueStore,
		gmailrelay.WithSyncerLogger(logger),
	)
	if err != nil {
		return err
	}
	forwarder, err := gmailrelay.NewForwardWorker(
		config,
		queueStore,
		gmailrelay.WithForwardWorkerLogger(logger),
	)
	if err != nil {
		return err
	}

	rootContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      gracefulShutdownTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- syncer.Run(rootContext) }()
	go func() { errorsCh <- forwarder.Run(rootContext) }()
	go func() {
		logger.Info("Gmail relay listening", "address", config.ListenAddr)
		errorsCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-rootContext.Done():
	case runtimeErr := <-errorsCh:
		if !errors.Is(runtimeErr, context.Canceled) && !errors.Is(runtimeErr, http.ErrServerClosed) {
			return runtimeErr
		}
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancelShutdown()
	return httpServer.Shutdown(shutdownCtx)
}
