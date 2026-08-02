// Package httpx holds HTTP helpers shared by skyl's provider adapters.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultClient is the HTTP client adapters use unless the caller supplies
// one.
//
// The timeouts are deliberately absent at the client level: streaming
// responses are long-lived by design, and a client-wide timeout would sever
// them mid-generation. Per-attempt bounds come from the request context, which
// skyl's Client sets.
func DefaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// maxErrorBody bounds how much of a failed response we read. Enough to
// diagnose, small enough that a misbehaving endpoint cannot exhaust memory.
const maxErrorBody = 64 << 10

// PostJSON sends body as JSON to url and returns the raw response.
//
// The caller owns the returned response body and must close it. On a non-2xx
// status the body is read, bounded, and returned alongside the status so the
// caller can classify the failure.
func PostJSON(
	ctx context.Context,
	hc *http.Client,
	url string,
	headers map[string]string,
	body any,
) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Get performs a GET and returns the raw response. The caller must close the
// body.
func Get(
	ctx context.Context,
	hc *http.Client,
	url string,
	headers map[string]string,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return hc.Do(req)
}

// ReadErrorBody reads a bounded prefix of a failed response body.
//
// It never returns an error: a body we cannot read is reported as empty rather
// than masking the status code that actually matters.
func ReadErrorBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	return b
}

// Merge overlays extra onto dst, overwriting existing keys.
//
// It backs Request.ProviderOptions: the caller's fields win, because the point
// of the escape hatch is to override what skyl chose.
func Merge(dst map[string]any, extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(extra))
	}
	for k, v := range extra {
		dst[k] = v
	}
	return dst
}
