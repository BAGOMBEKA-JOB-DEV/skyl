// Command skyl-gateway serves skyl over HTTP.
//
// Providers are registered from whichever API keys are present in the
// environment. An auth token is mandatory: the gateway proxies paid APIs, so
// it refuses to start as an open relay.
//
//	export SKYL_AUTH_TOKEN=$(openssl rand -hex 32)
//	export ANTHROPIC_API_KEY=sk-ant-...
//	skyl-gateway
//
// Configuration is documented in docs/gateway.md.
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

	"github.com/BAGOMBEKA-JOB-DEV/skyl/gateway"
)

// shutdownGrace bounds how long in-flight requests may finish after a signal.
// Long enough for a normal completion, short enough not to wedge a deploy.
const shutdownGrace = 30 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := gateway.ConfigFromEnv(logger)
	if err != nil {
		return err
	}

	srv, err := gateway.NewServer(cfg)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:    gateway.Addr(),
		Handler: srv,
		// No WriteTimeout: streaming responses are long-lived by design and a
		// write deadline would sever them mid-generation. ReadHeaderTimeout
		// still guards against slow-header attacks.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", httpSrv.Addr,
			"providers", srv.Providers(),
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
