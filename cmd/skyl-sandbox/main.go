// Command skyl-sandbox serves every provider's wire protocol locally, with no
// credentials and no cost.
//
//	go run ./cmd/skyl-sandbox
//
// One server hosts all four adapters, each at its own prefix:
//
//	anthropic     http://localhost:8099/anthropic
//	openai        http://localhost:8099/openai/v1
//	gemini        http://localhost:8099/gemini/v1beta
//	openaicompat  http://localhost:8099/compat/v1
//
// Point an adapter at one and everything works as it would against the real
// host — minus the model, the bill, and the key:
//
//	p := openai.New("sandbox-key",
//		openai.WithBaseURL("http://localhost:8099/openai/v1"))
//
// A model ID of "sandbox-status-429" makes the sandbox return that HTTP status
// in the provider's own error shape, which is how retry and error handling get
// exercised on demand.
//
// This is a development tool. It does not authenticate anything meaningfully,
// and it must not be exposed to a network you do not control.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/sandbox"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099",
		"listen address; loopback by default because this server is not a security boundary")
	apiKey := flag.String("api-key", sandbox.DefaultAPIKey,
		"credential the sandbox accepts")
	flag.Parse()

	h := sandbox.New(sandbox.WithAPIKey(*apiKey))

	srv := &http.Server{
		Addr:    *addr,
		Handler: h,
		// No WriteTimeout: streaming responses are long-lived by design.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "skyl sandbox listening on http://%s\n", *addr)
	fmt.Fprintf(os.Stderr, "  api key       %s\n", *apiKey)
	for _, m := range []struct{ name, path string }{
		{"anthropic", "/anthropic"},
		{"openai", "/openai/v1"},
		{"gemini", "/gemini/v1beta"},
		{"openaicompat", "/compat/v1"},
	} {
		fmt.Fprintf(os.Stderr, "  %-13s http://%s%s\n", m.name, *addr, m.path)
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("skyl-sandbox: %v", err)
	}
}
