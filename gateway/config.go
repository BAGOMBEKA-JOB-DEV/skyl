package gateway

import (
	"log/slog"
	"os"
	"strconv"
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
)

// DefaultAddr is the listen address used when SKYL_ADDR is unset.
const DefaultAddr = ":8080"

// ConfigFromEnv builds a [Config] from the environment.
//
// A provider is registered for each API key present, so an operator controls
// the provider set purely through the environment. It returns an error if no
// provider key is set or the auth token is missing — both are misconfigurations
// that should stop startup rather than surface as confusing 404s later.
func ConfigFromEnv(logger *slog.Logger) (Config, error) {
	providers := map[string]*skyl.Client{}

	if key := os.Getenv(EnvAnthropicKey); key != "" {
		providers["anthropic"] = skyl.New(anthropic.New(key))
	}
	if key := os.Getenv(EnvOpenAIKey); key != "" {
		providers["openai"] = skyl.New(openai.New(key))
	}
	if key := os.Getenv(EnvGeminiKey); key != "" {
		providers["gemini"] = skyl.New(gemini.New(key))
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
		))
	}

	cfg := Config{
		Providers:       providers,
		DefaultProvider: os.Getenv(EnvDefaultProvider),
		AuthToken:       os.Getenv(EnvAuthToken),
		Logger:          logger,
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

	return cfg, nil
}

// Addr returns the configured listen address, or [DefaultAddr].
func Addr() string {
	if a := os.Getenv(EnvAddr); a != "" {
		return a
	}
	return DefaultAddr
}
