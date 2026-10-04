// Command graph-stub serves the synthetic Meta Graph API that local runs, CI
// and staging point whatsapp.base_url at. It is configured only through the
// STUB_* environment (see package graphstub) and refuses to start outside the
// local and staging environments.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shridarpatil/whatomate/release/staging/graphstub"
)

const shutdownTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		// Config errors name a variable, never its value.
		logger.Error("graph stub stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := graphstub.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	stub, err := graphstub.New(config, logger)
	if err != nil {
		return err
	}
	defer stub.Close()

	server := &http.Server{
		Addr:              config.ListenAddr,
		Handler:           stub,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	served := make(chan error, 1)
	go func() {
		served <- server.ListenAndServe()
	}()
	logger.Info("graph stub listening", "environment", config.Environment, "addr", config.ListenAddr,
		"accounts", len(config.Accounts), "access_tokens", len(config.AccessTokens))

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listener failed: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.New("shutdown did not complete")
	}
	return nil
}
