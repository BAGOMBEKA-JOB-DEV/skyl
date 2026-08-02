// Package openaicompat adapts any endpoint that speaks OpenAI's
// chat-completions wire format.
//
// Much of the industry serves that shape, so one adapter plus a base URL
// reaches a long tail of hosts with no per-vendor code:
//
//	xAI (Grok)   https://api.x.ai/v1
//	DeepSeek     https://api.deepseek.com/v1
//	Mistral      https://api.mistral.ai/v1
//	Groq         https://api.groq.com/openai/v1
//	Together     https://api.together.xyz/v1
//	Fireworks    https://api.fireworks.ai/inference/v1
//	OpenRouter   https://openrouter.ai/api/v1
//	Perplexity   https://api.perplexity.ai
//	Cerebras     https://api.cerebras.ai/v1
//	DeepInfra    https://api.deepinfra.com/v1/openai
//	Qwen         https://dashscope.aliyuncs.com/compatible-mode/v1
//	Moonshot     https://api.moonshot.cn/v1
//	Nvidia NIM   https://integrate.api.nvidia.com/v1
//	Ollama       http://localhost:11434/v1
//	vLLM         http://localhost:8000/v1
//	LM Studio    http://localhost:1234/v1
//	llama.cpp    http://localhost:8080/v1
//
// These hosts implement OpenAI's *format*, not necessarily its *features*.
// Tool calling, streaming, and multimodal support vary by host and by model.
// Where a host rejects something, you get that host's error, classified —
// not a skyl-invented one.
package openaicompat

import (
	"context"
	"net/http"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/oai"
)

// Provider adapts an OpenAI-compatible endpoint. It is safe for concurrent
// use.
type Provider struct {
	client *oai.Client
}

// Option configures a [Provider].
type Option func(*oai.Config)

// WithBaseURL sets the API root, without a trailing slash. Required.
func WithBaseURL(url string) Option {
	return func(c *oai.Config) { c.BaseURL = url }
}

// WithAPIKey sets the bearer token.
//
// Omit it for local runtimes such as Ollama, LM Studio, and llama.cpp, which
// need no credential.
func WithAPIKey(key string) Option {
	return func(c *oai.Config) { c.APIKey = key }
}

// WithName sets the provider name reported in responses, errors, and hook
// events. Defaults to "openai-compatible"; set something specific so your
// metrics can tell hosts apart.
func WithName(name string) Option {
	return func(c *oai.Config) { c.Name = name }
}

// WithHTTPClient supplies the HTTP client, for custom transports, proxies,
// instrumentation, or a private trust store.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *oai.Config) { c.HTTPClient = hc }
}

// WithHeader adds a header to every request. Some hosts require one —
// OpenRouter attributes traffic through HTTP-Referer and X-Title.
func WithHeader(key, value string) Option {
	return func(c *oai.Config) {
		if c.ExtraHeaders == nil {
			c.ExtraHeaders = make(map[string]string)
		}
		c.ExtraHeaders[key] = value
	}
}

// WithMaxTokensField overrides the field carrying [skyl.Request.MaxTokens].
//
// Defaults to "max_tokens", which is what compatible hosts overwhelmingly
// expect. Use "max_completion_tokens" for hosts that have followed OpenAI's
// newer models.
func WithMaxTokensField(field string) Option {
	return func(c *oai.Config) { c.MaxTokensField = field }
}

// New returns a Provider for an OpenAI-compatible endpoint.
//
// [WithBaseURL] is required:
//
//	p := openaicompat.New(
//		openaicompat.WithBaseURL("https://api.groq.com/openai/v1"),
//		openaicompat.WithAPIKey(os.Getenv("GROQ_API_KEY")),
//		openaicompat.WithName("groq"),
//	)
//
// It panics if no base URL is given, because a provider that cannot reach a
// host is a programmer error, not a runtime condition.
func New(opts ...Option) *Provider {
	cfg := oai.Config{Name: "openai-compatible"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.BaseURL == "" {
		panic("skyl/openaicompat: New requires WithBaseURL")
	}
	return &Provider{client: oai.New(cfg)}
}

// Name returns the configured provider name.
func (p *Provider) Name() string { return p.client.Name() }

// Complete runs a request to completion.
func (p *Provider) Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error) {
	return p.client.Complete(ctx, req)
}

// Stream runs a request, returning events as the model produces them.
func (p *Provider) Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error) {
	return p.client.Stream(ctx, req)
}

// Models lists the host's models.
//
// Not every compatible host implements the models endpoint; those that do not
// return a classified error rather than an empty list, so you can tell "none"
// from "cannot ask".
func (p *Provider) Models(ctx context.Context) ([]skyl.ModelInfo, error) {
	return p.client.Models(ctx)
}

// Verify at compile time that Provider satisfies the interface.
var _ skyl.Provider = (*Provider)(nil)
