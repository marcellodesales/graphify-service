// Command graphify-api is the REST + MCP control-plane API for
// Graphify-as-a-Service (spec docs/FEATURES-BACKEND-SERVICE.md §6, §12).
//
// Phase 1 implements the REST surface (submit/list/status) backed by the
// filesystem metadata store, plus health/readiness. NATS publication and the
// MCP transport arrive in later phases.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcellodesales/graphify-service/backend/internal/api"
	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/repository"
	"github.com/marcellodesales/graphify-service/backend/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := telemetry.NewLogger(cfg.LogLevel)

	store, err := repository.NewStore(cfg.ReposRoot)
	if err != nil {
		return err
	}

	srv := api.NewServer(cfg, store, logger)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening",
			"addr", cfg.HTTPAddr,
			"repos_root", cfg.ReposRoot,
			"auth_mode", string(cfg.AuthMode),
			"env", cfg.Env,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
