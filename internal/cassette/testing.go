package cassette

import (
	"net/http"
	"path/filepath"
	"testing"
)

// Dir is where recordings live, relative to the package under test.
const Dir = "testdata/cassettes"

// Transport returns an http.RoundTripper backed by the named cassette, and a
// function that must be called when the exchange is finished.
//
// In replay mode — the default, and what CI runs — no network access happens
// and the returned closer is a no-op. With SKYL_RECORD=1 the request goes to
// the real provider and the closer writes the scrubbed exchange to disk.
//
// The name becomes the file name, so use a path-ish one: "openai/complete".
func Transport(t *testing.T, name string) (http.RoundTripper, func()) {
	t.Helper()

	path := filepath.Join(Dir, name+".json")

	if IsRecording() {
		rt := &roundTripper{path: path, recording: true, next: http.DefaultTransport}
		return rt, func() {
			t.Helper()
			rt.mu.Lock()
			rec := rt.rec
			rt.mu.Unlock()
			if rec == nil {
				t.Fatalf("cassette: nothing was recorded for %q; the adapter made no request", name)
			}
			if err := Save(path, rec); err != nil {
				t.Fatalf("cassette: saving %s: %v", path, err)
			}
			t.Logf("cassette: recorded %s", path)
		}
	}

	rec, err := Load(path)
	if err != nil {
		t.Skipf("cassette %s is not recorded yet — run with %s=1 and a provider key to create it",
			path, RecordEnv)
	}
	return &roundTripper{path: path, rec: rec}, func() {}
}

// Client returns an *http.Client wired to the named cassette. Every adapter
// accepts one through its WithHTTPClient option, which is the single seam all
// four share.
func Client(t *testing.T, name string) (*http.Client, func()) {
	t.Helper()
	rt, done := Transport(t, name)
	return &http.Client{Transport: rt}, done
}
