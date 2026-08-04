package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/anthropic"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

// Environment variables the gateway reads. Documented in docs/gateway.md.
const (
	EnvAddr            = "SKYL_ADDR"
	EnvAuthToken       = "SKYL_AUTH_TOKEN"
	EnvDefaultProvider = "SKYL_DEFAULT_PROVIDER"
	EnvRequestTimeout  = "SKYL_REQUEST_TIMEOUT"
	EnvIncludeRaw      = "SKYL_INCLUDE_RAW"

	EnvAnthropicKey = "ANTHROPIC_API_KEY"
	EnvOpenAIKey    = "OPENAI_API_KEY"
	EnvGeminiKey    = "GEMINI_API_KEY"

	EnvCompatName    = "SKYL_COMPAT_NAME"
	EnvCompatBaseURL = "SKYL_COMPAT_BASE_URL"
	EnvCompatKey     = "SKYL_COMPAT_API_KEY"

	// Retry and timeout behaviour. Every client was previously constructed
	// with bare defaults — 3 retries, a 5-minute Retry-After cap and a
	// 10-minute per-attempt timeout — none of which an operator could see or
	// change. A gateway retries behind a caller who is already waiting and may
	// be retrying too, so those numbers multiply.
	EnvMaxRetries     = "SKYL_MAX_RETRIES"
	EnvRetryBase      = "SKYL_RETRY_BASE_DELAY"
	EnvRetryMax       = "SKYL_RETRY_MAX_DELAY"
	EnvRetryAfterCap  = "SKYL_RETRY_AFTER_CAP"
	EnvAttemptTimeout = "SKYL_ATTEMPT_TIMEOUT"

	// Server behaviour.
	EnvAuthTokens        = "SKYL_AUTH_TOKENS"
	EnvMaxConcurrent     = "SKYL_MAX_CONCURRENT"
	EnvAllowedOrigins    = "SKYL_ALLOWED_ORIGINS"
	EnvHeartbeatInterval = "SKYL_HEARTBEAT_INTERVAL"

	// EnvMetrics enables the Prometheus /metrics endpoint. Off by default:
	// exposing it is a deployment decision, and an estate running an OTLP
	// collector wants neither the endpoint nor its dependency.
	EnvMetrics = "SKYL_METRICS"
)

// DefaultAddr is the listen address used when SKYL_ADDR is unset.
const DefaultAddr = ":8080"

// ConfigFromEnv builds a [Config] from the environment.
//
// A provider is registered for each API key present, so an operator controls
// the provider set purely through the environment.
//
// It returns an error only for values it cannot parse. The checks that stop
// startup — no provider registered, no auth token — live in [NewServer], so
// they apply equally to a Config built by hand.
func ConfigFromEnv(logger *slog.Logger) (Config, error) {
	clientOpts, err := clientOptionsFromEnv()
	if err != nil {
		return Config{}, err
	}

	var metricsHandler http.Handler
	if raw := os.Getenv(EnvMetrics); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvMetrics, err)
		}
		if on {
			tel, err := NewTelemetry()
			if err != nil {
				return Config{}, err
			}
			// The hook goes on every client, so the metrics cover all
			// providers rather than whichever one happened to be wired first.
			clientOpts = append(clientOpts, tel.Hook)
			metricsHandler = tel.Handler
		}
	}

	providers := map[string]*skyl.Client{}

	if key := os.Getenv(EnvAnthropicKey); key != "" {
		providers["anthropic"] = skyl.New(anthropic.New(key), clientOpts...)
	}
	if key := os.Getenv(EnvOpenAIKey); key != "" {
		providers["openai"] = skyl.New(openai.New(key), clientOpts...)
	}
	if key := os.Getenv(EnvGeminiKey); key != "" {
		providers["gemini"] = skyl.New(gemini.New(key), clientOpts...)
	}
	if baseURL := os.Getenv(EnvCompatBaseURL); baseURL != "" {
		name := os.Getenv(EnvCompatName)
		if name == "" {
			name = "compat"
		}
		providers[name] = skyl.New(openaicompat.New(
			openaicompat.WithBaseURL(baseURL),
			openaicompat.WithAPIKey(os.Getenv(EnvCompatKey)),
			openaicompat.WithName(name),
		), clientOpts...)
	}

	cfg := Config{
		Providers:       providers,
		DefaultProvider: os.Getenv(EnvDefaultProvider),
		AuthToken:       os.Getenv(EnvAuthToken),
		Logger:          logger,
		MetricsHandler:  metricsHandler,
	}

	if raw := os.Getenv(EnvRequestTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.RequestTimeout = d
	}
	if raw := os.Getenv(EnvIncludeRaw); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.IncludeRaw = v
	}
	if raw := os.Getenv(EnvMaxConcurrent); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvMaxConcurrent, err)
		}
		cfg.MaxConcurrent = n
	}
	if raw := os.Getenv(EnvHeartbeatInterval); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvHeartbeatInterval, err)
		}
		cfg.HeartbeatInterval = d
	}
	if raw := os.Getenv(EnvAllowedOrigins); raw != "" {
		cfg.AllowedOrigins = splitList(raw)
	}
	if raw := os.Getenv(EnvAuthTokens); raw != "" {
		tokens, err := parseLabelledTokens(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.AuthTokens = tokens
	}

	return cfg, nil
}

// clientOptionsFromEnv builds the skyl.Options every provider client gets.
func clientOptionsFromEnv() ([]skyl.Option, error) {
	var opts []skyl.Option

	if raw := os.Getenv(EnvMaxRetries); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvMaxRetries, err)
		}
		opts = append(opts, skyl.WithMaxRetries(n))
	}

	// The two backoff bounds are one option, so they are read together and
	// only applied when at least one is set.
	base, err := durationEnv(EnvRetryBase)
	if err != nil {
		return nil, err
	}
	maxDelay, err := durationEnv(EnvRetryMax)
	if err != nil {
		return nil, err
	}
	if base > 0 || maxDelay > 0 {
		opts = append(opts, skyl.WithRetryDelay(base, maxDelay))
	}

	retryAfterCap, err := durationEnv(EnvRetryAfterCap)
	if err != nil {
		return nil, err
	}
	if retryAfterCap > 0 {
		opts = append(opts, skyl.WithRetryAfterCap(retryAfterCap))
	}

	timeout, err := durationEnv(EnvAttemptTimeout)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		opts = append(opts, skyl.WithTimeout(timeout))
	}

	return opts, nil
}

func durationEnv(name string) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// splitList parses a comma-separated list, ignoring blanks and surrounding
// space.
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// parseLabelledTokens reads "label:token,label:token".
//
// The label is what appears in logs and metrics; the token never does.
func parseLabelledTokens(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range splitList(raw) {
		label, token, ok := strings.Cut(pair, ":")
		label, token = strings.TrimSpace(label), strings.TrimSpace(token)
		if !ok || label == "" || token == "" {
			return nil, fmt.Errorf("%s: want label:token pairs, got %q", EnvAuthTokens, pair)
		}
		out[label] = token
	}
	return out, nil
}

// Addr returns the configured listen address, or [DefaultAddr].
func Addr() string {
	if a := os.Getenv(EnvAddr); a != "" {
		return a
	}
	return DefaultAddr
}
