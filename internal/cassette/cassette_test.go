package cassette

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/testutil"
)

const secret = "sk-ant-super-secret-value"

func TestScrubRemovesEveryCredentialHeader(t *testing.T) {
	t.Parallel()

	rec := &Recording{
		RequestHeaders: map[string]string{
			"Authorization":       "Bearer " + secret,
			"X-Api-Key":           secret,
			"X-Goog-Api-Key":      secret,
			"Openai-Organization": "org-123",
			"Openai-Project":      "proj-123",
			"Content-Type":        "application/json",
		},
		ResponseHeaders: map[string]string{
			"Request-Id":                        "req_abc",
			"Anthropic-Organization-Id":         "org_xyz",
			"Anthropic-Ratelimit-Requests-Left": "42",
			"X-Ratelimit-Remaining-Tokens":      "999",
			"Set-Cookie":                        "session=abc",
			"Content-Type":                      "text/event-stream",
		},
	}

	Scrub(rec)

	for _, name := range []string{
		"Authorization", "X-Api-Key", "X-Goog-Api-Key",
		"Openai-Organization", "Openai-Project",
	} {
		if got := rec.RequestHeaders[name]; got != Redacted {
			t.Errorf("%s = %q, want it redacted", name, got)
		}
	}
	// Non-secret headers must survive: they are part of what makes a replay
	// faithful.
	if rec.RequestHeaders["Content-Type"] != "application/json" {
		t.Error("Content-Type was scrubbed; only credentials should be")
	}

	for _, name := range []string{
		"Request-Id", "Anthropic-Organization-Id",
		"Anthropic-Ratelimit-Requests-Left", "X-Ratelimit-Remaining-Tokens",
		"Set-Cookie",
	} {
		if _, ok := rec.ResponseHeaders[name]; ok {
			t.Errorf("response header %s survived; it identifies the account", name)
		}
	}
	if rec.ResponseHeaders["Content-Type"] != "text/event-stream" {
		t.Error("Content-Type was dropped; the replayed response needs it")
	}
}

// Scrubbing happens on write, not on read: docs/rules.md §7.2 requires
// redaction at construction, and a file is a durable log.
func TestSaveScrubsBeforeWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "openai", "complete.json")
	err := Save(path, &Recording{
		Method:         http.MethodPost,
		URL:            "https://api.openai.com/v1/chat/completions",
		RequestHeaders: map[string]string{"Authorization": "Bearer " + secret},
		Status:         200,
		ResponseBody:   `{"ok":true}`,
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the credential reached disk; scrubbing must happen before writing")
	}
	if !strings.Contains(string(raw), Redacted) {
		t.Error("the redaction marker is absent, so a leak would not be greppable")
	}
}

// A round trip through record and replay must reproduce the response exactly,
// including the streaming content type — the Anthropic SDK selects its decoder
// from it, so a lost header turns a stream into a parse failure.
func TestRecordThenReplay(t *testing.T) {
	t.Parallel()

	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("upstream saw Authorization = %q, want the real credential", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_should_not_persist")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)

	rt := &roundTripper{path: "unused", recording: true, next: http.DefaultTransport}

	req, err := http.NewRequestWithContext(testutil.Context(t), http.MethodPost, upstream.URL, strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("record RoundTrip: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != body {
		t.Errorf("recording pass returned %q, want the body passed through unchanged", got)
	}

	// The stored exchange must be clean...
	if rt.rec.RequestHeaders["Authorization"] != Redacted {
		t.Error("the credential was stored")
	}
	if _, ok := rt.rec.ResponseHeaders["Request-Id"]; ok {
		t.Error("Request-Id was stored")
	}

	// ...and replaying it must reproduce the response.
	replayer := &roundTripper{path: "unused", rec: rt.rec}
	replayed, err := replayer.RoundTrip(req)
	if err != nil {
		t.Fatalf("replay RoundTrip: %v", err)
	}
	defer replayed.Body.Close() //nolint:errcheck // test cleanup

	if replayed.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", replayed.StatusCode)
	}
	if ct := replayed.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want it preserved", ct)
	}
	data, _ := io.ReadAll(replayed.Body)
	if string(data) != body {
		t.Errorf("replayed body = %q, want %q", data, body)
	}
}

// Replaying without a recording must say what to do, not fail obscurely.
func TestReplayWithoutRecordingExplainsItself(t *testing.T) {
	t.Parallel()

	rt := &roundTripper{path: "testdata/cassettes/missing.json"}
	req, _ := http.NewRequestWithContext(testutil.Context(t), http.MethodGet, "https://example.com", nil)

	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want a missing-cassette error")
	}
	if !strings.Contains(err.Error(), RecordEnv) {
		t.Errorf("err = %q, want it to name %s so the reader knows how to fix it", err, RecordEnv)
	}
}

// The guard that matters once cassettes are committed: no fixture may contain
// anything credential-shaped. It scans whatever is present, so it starts
// passing vacuously and becomes real the moment the first recording lands.
func TestCommittedCassettesCarryNoCredentials(t *testing.T) {
	t.Parallel()

	// Prefixes real provider keys use. A recording that still contains one of
	// these was scrubbed incompletely.
	prefixes := []string{"sk-", "sk-ant-", "AIza", "gsk_", "xai-", "Bearer "}

	root := filepath.Join("..", "..")
	var checked int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil //nolint:nilerr // an unreadable tree is not this test's business
		}
		if !strings.Contains(filepath.ToSlash(path), "testdata/cassettes/") {
			return nil
		}
		checked++

		data, err := os.ReadFile(path) //nolint:gosec // walking our own testdata
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			return nil
		}
		var rec Recording
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Errorf("%s is not a valid recording: %v", path, err)
			return nil
		}
		for name, value := range rec.RequestHeaders {
			if secretRequestHeaders[http.CanonicalHeaderKey(name)] && value != Redacted {
				t.Errorf("%s: header %s is not redacted", path, name)
			}
		}
		for _, p := range prefixes {
			if strings.Contains(string(data), p) && !strings.Contains(string(data), Redacted) {
				t.Errorf("%s contains %q and no redaction marker; check it by hand", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking testdata: %v", err)
	}
	t.Logf("checked %d committed cassettes", checked)
}
