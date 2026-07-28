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
	if err := run(logger); err != nil {
		// Keep this final boundary static. Individual components emit
		// allow-listed operational context, while arbitrary startup/runtime
		// errors can contain credentials, provider bodies, or connection URLs.
		logger.Error("Meta relay stopped", "reason", safeStartupReason(err))
		os.Exit(1)
	}
}

func safeStartupReason(_ error) string {
	return "startup_or_runtime_failure"
}

func run(logger *slog.Logger) error {
	config, err := metarelay.LoadConfig()
	if err != nil {
		return err
	}
	store, err := metarelay.NewRedisStore(config)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	startupContext, startupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startupCancel()
	if err := store.Ping(startupContext); err != nil {
		return errors.New("redis durability boundary is unavailable")
	}

	server, err := metarelay.NewServer(
		config,
		store,
		metarelay.WithServerLogger(logger),
	)
	if err != nil {
		return err
	}
	worker, err := metarelay.NewWorker(
		config,
		store,
		metarelay.WithWorkerLogger(logger),
	)
	if err != nil {
		return err
	}

	rootContext, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	httpServer := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      metaRelayGracefulShutdownTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	workerErrors := make(chan error, 1)
	httpErrors := make(chan error, 1)
	go func() {
		workerErrors <- worker.Run(rootContext)
	}()
	go func() {
		logger.Info("Meta relay listening", "address", config.ListenAddr)
		httpErrors <- httpServer.ListenAndServe()
	}()

	select {
	case <-rootContext.Done():
	case err := <-workerErrors:
		if !errors.Is(err, context.Canceled) {
			return err
		}
	case err := <-httpErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownContext, cancel := context.WithTimeout(
		context.Background(),
		metaRelayGracefulShutdownTimeout,
	)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return err
	}
	return nil
}
