package gateway

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// These tests use t.Setenv, which is incompatible with t.Parallel — Go fails
// the test if both are used, because environment variables are process-global.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// clearEnv unsets every variable ConfigFromEnv reads, so a test sees only what
// it sets itself rather than inheriting the developer's real API keys.
//
// t.Setenv restores the previous value on cleanup, so this is safe even when
// the developer genuinely has ANTHROPIC_API_KEY exported.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvAddr, EnvAuthToken, EnvDefaultProvider, EnvRequestTimeout, EnvIncludeRaw,
		EnvAnthropicKey, EnvOpenAIKey, EnvGeminiKey,
		EnvCompatName, EnvCompatBaseURL, EnvCompatKey,
	} {
		t.Setenv(k, "")
	}
}

func TestConfigFromEnvRegistersProviderPerKey(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAnthropicKey, "sk-ant-test")
	t.Setenv(EnvOpenAIKey, "sk-openai-test")
	t.Setenv(EnvGeminiKey, "gemini-test")
	t.Setenv(EnvAuthToken, testToken)

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	for _, name := range []string{"anthropic", "openai", "gemini"} {
		if _, ok := cfg.Providers[name]; !ok {
			t.Errorf("provider %q not registered", name)
		}
	}
	if len(cfg.Providers) != 3 {
		t.Errorf("got %d providers, want 3", len(cfg.Providers))
	}
	if cfg.AuthToken != testToken {
		t.Errorf("AuthToken = %q", cfg.AuthToken)
	}

	// Providers() feeds the startup log and the /providers endpoint, so the
	// order must be stable rather than map-iteration order.
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	got := srv.Providers()
	want := []string{"anthropic", "gemini", "openai"}
	if len(got) != len(want) {
		t.Fatalf("Providers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Providers() = %v, want %v", got, want)
			break
		}
	}
}

// TestConfigFromEnvRegistersNothingWithoutKeys is the flip side: an absent key
// must not quietly register a provider that would then fail on every call.
func TestConfigFromEnvRegistersNothingWithoutKeys(t *testing.T) {
	clearEnv(t)

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("got %d providers with no keys set, want 0", len(cfg.Providers))
	}
}

func TestConfigFromEnvCompatProvider(t *testing.T) {
	tests := []struct {
		name     string
		envName  string
		wantName string
	}{
		{"explicit name", "groq", "groq"},
		{"default name", "", "compat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvCompatBaseURL, "https://api.groq.com/openai/v1")
			t.Setenv(EnvCompatKey, "gsk-test")
			t.Setenv(EnvCompatName, tt.envName)

			cfg, err := ConfigFromEnv(discardLogger())
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if _, ok := cfg.Providers[tt.wantName]; !ok {
				t.Errorf("compat provider not registered as %q; got %v",
					tt.wantName, providerNames(cfg))
			}
		})
	}
}

// TestConfigFromEnvCompatNeedsBaseURL guards the panic in openaicompat.New: a
// key without a base URL must skip registration rather than crash the gateway
// at startup.
func TestConfigFromEnvCompatNeedsBaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvCompatKey, "gsk-test")
	t.Setenv(EnvCompatName, "groq")

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("registered %v without a compat base URL", providerNames(cfg))
	}
}

func TestConfigFromEnvParsesValues(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvOpenAIKey, "sk-test")
	t.Setenv(EnvAuthToken, testToken)
	t.Setenv(EnvDefaultProvider, "openai")
	t.Setenv(EnvRequestTimeout, "45s")
	t.Setenv(EnvIncludeRaw, "true")

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.RequestTimeout != 45*time.Second {
		t.Errorf("RequestTimeout = %v, want 45s", cfg.RequestTimeout)
	}
	if !cfg.IncludeRaw {
		t.Error("IncludeRaw = false, want true")
	}
	if cfg.DefaultProvider != "openai" {
		t.Errorf("DefaultProvider = %q", cfg.DefaultProvider)
	}
}

// TestConfigFromEnvRejectsMalformedValues checks a typo stops startup instead
// of silently falling back to a default the operator did not choose.
func TestConfigFromEnvRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"timeout", EnvRequestTimeout, "not-a-duration"},
		{"include raw", EnvIncludeRaw, "yes-please"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvOpenAIKey, "sk-test")
			t.Setenv(tt.key, tt.value)

			if _, err := ConfigFromEnv(discardLogger()); err == nil {
				t.Errorf("%s=%q was accepted", tt.key, tt.value)
			}
		})
	}
}

// TestConfigFromEnvRefusesOpenRelay is the security-critical path: the gateway
// proxies paid APIs, so a config missing its auth token must not produce a
// running server. The check lives in NewServer, so this exercises the pair as
// an operator actually uses them.
func TestConfigFromEnvRefusesOpenRelay(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvOpenAIKey, "sk-test")
	// SKYL_AUTH_TOKEN deliberately unset.

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("NewServer accepted a config with no auth token")
	}
}

// TestConfigFromEnvRefusesNoProviders covers the other startup guard.
func TestConfigFromEnvRefusesNoProviders(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAuthToken, testToken)

	cfg, err := ConfigFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("NewServer accepted a config with no providers")
	}
}

func TestAddr(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"default", "", DefaultAddr},
		{"override", "127.0.0.1:9999", "127.0.0.1:9999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvAddr, tt.env)
			if got := Addr(); got != tt.want {
				t.Errorf("Addr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func providerNames(cfg Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		out = append(out, name)
	}
	return out
}

func TestClientOptionsFromEnv(t *testing.T) {
	// Not parallel: it sets process environment.
	t.Setenv(EnvOpenAIKey, "k")
	t.Setenv(EnvMaxRetries, "1")
	t.Setenv(EnvRetryBase, "10ms")
	t.Setenv(EnvRetryMax, "100ms")
	t.Setenv(EnvRetryAfterCap, "2s")
	t.Setenv(EnvAttemptTimeout, "30s")
	t.Setenv(EnvAuthToken, "t")

	cfg, err := ConfigFromEnv(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("got %d providers, want 1", len(cfg.Providers))
	}
	// The options are applied inside skyl.New, which exposes no getters — so
	// what is checked here is that a full set parses and builds a client at
	// all. The values themselves are skyl's to honour, and it has its own
	// tests for that.
	if cfg.Providers["openai"] == nil {
		t.Error("openai client was not built")
	}
}

func TestClientOptionsRejectMalformedValues(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"max retries", EnvMaxRetries, "many"},
		{"retry base", EnvRetryBase, "soon"},
		{"retry max", EnvRetryMax, "later"},
		{"retry-after cap", EnvRetryAfterCap, "never"},
		{"attempt timeout", EnvAttemptTimeout, "eventually"},
		{"max concurrent", EnvMaxConcurrent, "lots"},
		{"heartbeat", EnvHeartbeatInterval, "sometimes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvOpenAIKey, "k")
			t.Setenv(EnvAuthToken, "t")
			t.Setenv(tc.env, tc.value)

			if _, err := ConfigFromEnv(slog.New(slog.DiscardHandler)); err == nil {
				t.Errorf("%s = %q was accepted; a typo in a deployment must fail loudly",
					tc.env, tc.value)
			}
		})
	}
}

func TestServerSettingsFromEnv(t *testing.T) {
	t.Setenv(EnvOpenAIKey, "k")
	t.Setenv(EnvAuthToken, "primary")
	t.Setenv(EnvAuthTokens, "ci:token-a, partner:token-b")
	t.Setenv(EnvMaxConcurrent, "16")
	t.Setenv(EnvAllowedOrigins, "https://a.example.com, https://b.example.com")
	t.Setenv(EnvHeartbeatInterval, "5s")

	cfg, err := ConfigFromEnv(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}

	if cfg.MaxConcurrent != 16 {
		t.Errorf("MaxConcurrent = %d, want 16", cfg.MaxConcurrent)
	}
	if cfg.HeartbeatInterval != 5*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 5s", cfg.HeartbeatInterval)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != "https://a.example.com" {
		t.Errorf("AllowedOrigins = %v, want two trimmed entries", cfg.AllowedOrigins)
	}
	if cfg.AuthTokens["ci"] != "token-a" || cfg.AuthTokens["partner"] != "token-b" {
		t.Errorf("AuthTokens = %v, want ci and partner labelled", cfg.AuthTokens)
	}
}

func TestLabelledTokensRejectMalformedPairs(t *testing.T) {
	for _, raw := range []string{"no-colon", ":missing-label", "label:", "a:b,broken"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvOpenAIKey, "k")
			t.Setenv(EnvAuthToken, "t")
			t.Setenv(EnvAuthTokens, raw)

			if _, err := ConfigFromEnv(slog.New(slog.DiscardHandler)); err == nil {
				t.Errorf("%q was accepted; a malformed token list must not start silently", raw)
			}
		})
	}
}
