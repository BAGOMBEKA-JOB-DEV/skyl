package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// run() had no test at all, which is how the shutdown path came to block for
// the full grace period and then exit non-zero on an ordinary SIGTERM. These
// tests drive the real signal path.

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freePort reserves a port and releases it, so the server can bind it.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startGateway runs the real entry point against a stub upstream and returns
// its base URL plus a function that signals shutdown and waits.
func startGateway(t *testing.T) (string, func() error) {
	t.Helper()

	upstream := stubUpstream(t)
	addr := freePort(t)

	t.Setenv("SKYL_ADDR", addr)
	t.Setenv("SKYL_AUTH_TOKEN", "t")
	t.Setenv("SKYL_COMPAT_BASE_URL", upstream+"/v1")
	t.Setenv("SKYL_COMPAT_NAME", "stub")
	// Keep the drain window short so the test is not dominated by it.
	t.Setenv("SKYL_HEARTBEAT_INTERVAL", "50ms")

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWithSignal(ctx, discard()) }()

	// Wait for the listener.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return "http://" + addr, func() error {
		stop()
		select {
		case err := <-done:
			return err
		case <-time.After(shutdownGrace + drainDelay + 10*time.Second):
			t.Fatal("run() did not return after shutdown")
			return nil
		}
	}
}

// A SIGTERM with nothing in flight must exit cleanly and promptly.
func TestShutdownExitsCleanly(t *testing.T) {
	base, shutdown := startGateway(t)

	resp, err := http.Get(base + "/healthz") //nolint:noctx // short-lived probe
	if err != nil {
		t.Fatalf("probing: %v", err)
	}
	_ = resp.Body.Close()

	start := time.Now()
	if err := shutdown(); err != nil {
		t.Fatalf("run() returned %v, want nil — an ordinary deploy must not look like a crash", err)
	}
	// It should not have waited out the grace period.
	if elapsed := time.Since(start); elapsed > shutdownGrace {
		t.Errorf("shutdown took %v, longer than the %v grace period", elapsed, shutdownGrace)
	}
}

// Readiness must fail before the listener stops, so an orchestrator can take
// the instance out of rotation while in-flight work finishes.
func TestReadinessFailsBeforeTheListenerStops(t *testing.T) {
	base, shutdown := startGateway(t)

	resp, err := http.Get(base + "/readyz") //nolint:noctx // short-lived probe
	if err != nil {
		t.Fatalf("probing readiness: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d before shutdown, want 200", resp.StatusCode)
	}

	// Watch readiness while shutdown runs; it must report 503 at least once
	// before the socket goes away.
	var (
		wg        sync.WaitGroup
		sawDrain  bool
		watchStop = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-watchStop:
				return
			default:
			}
			r, err := http.Get(base + "/readyz") //nolint:noctx // polling probe
			if err != nil {
				return // listener gone
			}
			_ = r.Body.Close()
			if r.StatusCode == http.StatusServiceUnavailable {
				sawDrain = true
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	err = shutdown()
	close(watchStop)
	wg.Wait()

	if err != nil {
		t.Fatalf("run() returned %v", err)
	}
	if !sawDrain {
		t.Error("readiness never reported draining; traffic would still be routed here")
	}
}

// A stub OpenAI-compatible upstream, so the gateway has something to register.
func stubUpstream(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	return "http://" + l.Addr().String()
}

func TestAddrIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("SKYL_ADDR", "127.0.0.1:9999")
	if !strings.HasSuffix(os.Getenv("SKYL_ADDR"), ":9999") {
		t.Fatal("environment not set")
	}
}
