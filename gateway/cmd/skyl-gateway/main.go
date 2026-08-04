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

// drainDelay is how long readiness reports failure before the listener stops.
//
// It exists because a load balancer learns about readiness by polling. Closing
// the listener the instant we decide to stop means requests already in flight
// toward this instance arrive at a closed socket — a connection refused, which
// is what a crash looks like too. A short pause turns a deploy into a
// withdrawal.
const drainDelay = 2 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// The signal context is created here and the work happens in
	// runWithSignal, so a test can drive the whole shutdown path by cancelling
	// a context instead of sending itself a real signal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithSignal(ctx, logger)
}

func runWithSignal(ctx context.Context, logger *slog.Logger) error {
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
		logger.Info("draining")
	}

	// Fail readiness first, and give the orchestrator a moment to act on it
	// before the listener stops. Without this window, taking the instance out
	// of rotation and closing the listener happen at once, and whatever was
	// in flight at that instant is refused rather than drained.
	srv.StartDraining()
	time.Sleep(drainDelay)

	logger.Info("shutting down", "grace", shutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		// A stream still running at the end of the grace period is not a
		// crash. Shutdown waits for connections to go idle and never cancels a
		// request context, so a long generation reaches the deadline as a
		// matter of course — and exiting non-zero for it would make every
		// ordinary deploy look like a failure in the dashboards.
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("grace period expired with requests still in flight; closing anyway",
				"grace", shutdownGrace)
			return httpSrv.Close()
		}
		return err
	}
	logger.Info("shutdown complete")
	return nil
}
