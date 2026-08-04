// Package cassette records real provider HTTP exchanges and replays them.
//
// # Why this exists
//
// Every fake in this repository was written from the same provider
// documentation as the adapter it tests, so a wrong field name makes the fake
// wrong in the same way and both stay green. Only a real call settles it, and a
// real call needs a paid credential — which most contributors, and CI, do not
// have.
//
// A cassette closes that gap once. One contributor with a key records the
// exchange; everyone else replays it forever, offline and for free. The
// recording is a real provider response, so a field name that does not match
// reality fails on replay for everybody.
//
// # Standard library only
//
// docs/rules.md §4.1 gives the core module zero external dependencies, and
// internal/testutil says outright that the guarantee "applies to test helpers
// too". go-vcr appears in two go.sum files but only as the Anthropic SDK's own
// test dependency; adopting it would need an ADR and would put a recording
// library in everyone's dependency graph. This is a few hundred lines of
// net/http and encoding/json instead.
//
// # Using it
//
//	tr, done := cassette.Transport(t, "openai/complete")
//	defer done()
//	p := openai.New(key, openai.WithHTTPClient(&http.Client{Transport: tr}))
//
// With SKYL_RECORD=1 and a real key in the environment, the exchange is made
// for real and written to testdata. Without it, the recording is replayed and
// no network access happens at all.
//
// # What is stored
//
// Credentials are scrubbed on write, never on read, because a cassette is a
// durable log and docs/rules.md §7.2 requires redaction at construction. Note
// that a recorded request body contains whatever prompt was sent, and that ships
// in the repository permanently — record deliberately, not accidentally.
package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// RecordEnv enables recording. Without it, cassettes are replayed.
const RecordEnv = "SKYL_RECORD"

// Redacted replaces every scrubbed value, so a cassette that still contains a
// live credential is obvious on sight and greppable in CI.
const Redacted = "REDACTED"

// Recording is one HTTP exchange, as stored on disk.
type Recording struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	Status          int               `json:"status"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ResponseBody    string            `json:"response_body"`
}

// secretRequestHeaders carry credentials. Every one is replaced with Redacted.
//
// Canonical form, as net/http stores them.
var secretRequestHeaders = map[string]bool{
	"Authorization":       true, // openai, openaicompat
	"X-Api-Key":           true, // anthropic
	"X-Goog-Api-Key":      true, // gemini
	"Openai-Organization": true,
	"Openai-Project":      true,
	"Api-Key":             true, // azure deployments
}

// noisyResponseHeaders are account-identifying or vary per call. They are
// dropped rather than redacted: nothing replays them, and keeping an
// organisation ID in a public repository serves no one.
var noisyResponseHeaders = []string{
	"request-id", "x-request-id",
	"anthropic-organization-id", "openai-organization",
	"set-cookie", "cf-ray", "x-served-by",
	"date", "server",
}

// Scrub removes credentials from a recording. It is exported so a test can
// assert that what lands on disk is clean.
func Scrub(rec *Recording) {
	for name := range rec.RequestHeaders {
		if secretRequestHeaders[http.CanonicalHeaderKey(name)] {
			rec.RequestHeaders[name] = Redacted
		}
	}
	for _, drop := range noisyResponseHeaders {
		for name := range rec.ResponseHeaders {
			if strings.EqualFold(name, drop) {
				delete(rec.ResponseHeaders, name)
			}
		}
		// Rate-limit headers are a family rather than a fixed list.
		for name := range rec.ResponseHeaders {
			if strings.Contains(strings.ToLower(name), "ratelimit") {
				delete(rec.ResponseHeaders, name)
			}
		}
	}
}

// roundTripper replays a recording, or records one when recording is enabled.
type roundTripper struct {
	path string

	// mu guards rec: an adapter may issue several requests, and net/http can
	// run them from more than one goroutine.
	mu  sync.Mutex
	rec *Recording

	recording bool
	next      http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.recording {
		return t.record(req)
	}
	return t.replay(req)
}

func (t *roundTripper) replay(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	rec := t.rec
	t.mu.Unlock()

	if rec == nil {
		return nil, fmt.Errorf("cassette: no recording at %s; "+
			"re-record with %s=1 and a provider key set", t.path, RecordEnv)
	}

	header := make(http.Header, len(rec.ResponseHeaders))
	for k, v := range rec.ResponseHeaders {
		header.Set(k, v)
	}

	return &http.Response{
		Status:     http.StatusText(rec.Status),
		StatusCode: rec.Status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1,
		Header:  header,
		Body:    io.NopCloser(strings.NewReader(rec.ResponseBody)),
		Request: req,
	}, nil
}

func (t *roundTripper) record(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("cassette: reading request body: %w", err)
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// The whole body is buffered so it can be written to disk and handed back.
	// That defeats streaming for the recording pass, which is acceptable: the
	// replay pass is the one every contributor runs, and it streams from the
	// stored bytes exactly as a socket would.
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("cassette: reading response body: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	rec := &Recording{
		Method:          req.Method,
		URL:             req.URL.String(),
		RequestHeaders:  flatten(req.Header),
		RequestBody:     string(reqBody),
		Status:          resp.StatusCode,
		ResponseHeaders: flatten(resp.Header),
		ResponseBody:    string(respBody),
	}
	Scrub(rec)

	t.mu.Lock()
	t.rec = rec
	t.mu.Unlock()

	return resp, nil
}

func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// Load reads a recording from disk.
func Load(path string) (*Recording, error) {
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path, not user input
	if err != nil {
		return nil, err
	}
	var rec Recording
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("cassette: %s: %w", path, err)
	}
	return &rec, nil
}

// Save writes a recording, scrubbed, with stable formatting so a re-record
// produces a reviewable diff rather than a reshuffle.
func Save(path string, rec *Recording) error {
	Scrub(rec)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Names returns the recording names present under dir, sorted.
func Names(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// IsRecording reports whether the environment asks for a live re-record.
func IsRecording() bool { return os.Getenv(RecordEnv) != "" }
