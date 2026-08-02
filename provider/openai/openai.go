// Package openai adapts the OpenAI API.
//
// It reaches the GPT-5.6 family (Sol, Terra, Luna), GPT-5.5, GPT-5.4 nano, the
// o-series, and gpt-oss. Model IDs are passed through untouched, so a model
// released after your skyl build works immediately — see
// docs/adr/0004-model-ids-are-pass-through.md.
package openai

import (
	"context"
	"net/http"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/oai"
)

// DefaultBaseURL is OpenAI's API root.
const DefaultBaseURL = "https://api.openai.com/v1"

// Provider adapts the OpenAI API. It is safe for concurrent use.
type Provider struct {
	client *oai.Client
}

// Option configures a [Provider].
type Option func(*oai.Config)

// WithBaseURL overrides the API root — for a gateway, a proxy, or a
// compatible deployment.
func WithBaseURL(url string) Option {
	return func(c *oai.Config) { c.BaseURL = url }
}

// WithHTTPClient supplies the HTTP client, for custom transports, proxies,
// instrumentation, or a private trust store.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *oai.Config) { c.HTTPClient = hc }
}

// WithOrganization sets the OpenAI-Organization header.
func WithOrganization(org string) Option {
	return withHeader("OpenAI-Organization", org)
}

// WithProject sets the OpenAI-Project header.
func WithProject(project string) Option {
	return withHeader("OpenAI-Project", project)
}

// WithMaxTokensField overrides the field carrying [skyl.Request.MaxTokens].
//
// Defaults to "max_completion_tokens", which OpenAI's current models require.
// Set "max_tokens" when targeting an older deployment.
func WithMaxTokensField(field string) Option {
	return func(c *oai.Config) { c.MaxTokensField = field }
}

func withHeader(key, value string) Option {
	return func(c *oai.Config) {
		if value == "" {
			return
		}
		if c.ExtraHeaders == nil {
			c.ExtraHeaders = make(map[string]string)
		}
		c.ExtraHeaders[key] = value
	}
}

// New returns a Provider authenticating with apiKey.
//
//	p := openai.New(os.Getenv("OPENAI_API_KEY"))
func New(apiKey string, opts ...Option) *Provider {
	cfg := oai.Config{
		Name:    "openai",
		BaseURL: DefaultBaseURL,
		APIKey:  apiKey,
		// Current OpenAI models reject max_tokens in favour of this.
		MaxTokensField: "max_completion_tokens",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Provider{client: oai.New(cfg)}
}

// Name returns "openai".
func (p *Provider) Name() string { return p.client.Name() }

// Complete runs a request to completion.
func (p *Provider) Complete(ctx context.Context, req *skyl.Request) (*skyl.Response, error) {
	return p.client.Complete(ctx, req)
}

// Stream runs a request, returning events as the model produces them.
func (p *Provider) Stream(ctx context.Context, req *skyl.Request) (skyl.Stream, error) {
	return p.client.Stream(ctx, req)
}

// Models lists the models available to this account.
func (p *Provider) Models(ctx context.Context) ([]skyl.ModelInfo, error) {
	return p.client.Models(ctx)
}

// Verify at compile time that Provider satisfies the interface.
var _ skyl.Provider = (*Provider)(nil)
