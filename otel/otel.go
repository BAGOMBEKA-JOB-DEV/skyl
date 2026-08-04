// Package otel instruments skyl with OpenTelemetry.
//
// It turns skyl's hook events into spans and metrics following the
// OpenTelemetry GenAI semantic conventions, so model traffic shows up in an
// observability stack the same way any other dependency does — and looks the
// same whichever provider served it, which is the point of the library.
//
//	client := skyl.New(openai.New(key), otel.Hook())
//
// That is the whole integration for traces and metrics. To get the trace
// context onto the outbound HTTP request as well, wrap the transport:
//
//	client := skyl.New(
//		openai.New(key, openai.WithHTTPClient(otel.HTTPClient(nil))),
//		otel.Hook(),
//	)
//
// # A separate module
//
// This lives outside the core module because the OpenTelemetry SDK brings a
// dependency graph, and docs/rules.md §4.1 gives the core zero dependencies.
// Nobody who does not want OpenTelemetry pays for it — the same reasoning as
// ADR-0006 applies to the Anthropic SDK.
//
// # What is deliberately not recorded
//
// Prompt and completion content. The conventions allow capturing messages, and
// skyl gives this package the whole [skyl.Request] — but a span is a durable
// record shipped to a third-party backend, and putting user conversations there
// by default is a decision no library should make silently. Only the shape of
// the request is recorded: model, sampling parameters, token counts.
package otel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// ScopeName identifies this instrumentation in the telemetry it produces.
const ScopeName = "github.com/BAGOMBEKA-JOB-DEV/skyl/otel"

// Attribute and metric names from the GenAI semantic conventions.
//
// They are spelled out here rather than taken from a semconv package because
// the GenAI conventions are still in development: pinning a semconv module
// would tie skyl's release cadence to theirs, and these keys are stable enough
// to write down and cheap to correct.
const (
	attrOperationName = "gen_ai.operation.name"
	attrProviderName  = "gen_ai.provider.name"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrResponseID    = "gen_ai.response.id"
	attrFinishReasons = "gen_ai.response.finish_reasons"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrTemperature   = "gen_ai.request.temperature"
	attrTopP          = "gen_ai.request.top_p"
	attrMaxTokens     = "gen_ai.request.max_tokens"
	attrTokenType     = "gen_ai.token.type"
	attrErrorType     = "error.type"

	metricTokenUsage = "gen_ai.client.token.usage"
	metricDuration   = "gen_ai.client.operation.duration"
)

// config holds the providers the instrumentation writes to.
type config struct {
	tracer trace.Tracer
	meter  metric.Meter

	// recordSpans is false for a metrics-only setup. Spans on a high-volume
	// gateway are the expensive half.
	recordSpans bool
}

// Option configures the instrumentation.
type Option func(*config)

// WithTracerProvider sets the tracer provider. Defaults to the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracer = tp.Tracer(ScopeName)
		}
	}
}

// WithMeterProvider sets the meter provider. Defaults to the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meter = mp.Meter(ScopeName)
		}
	}
}

// WithoutSpans records metrics only.
//
// Worth reaching for on a busy gateway: the metrics are cheap and bounded,
// while one span per model call is not.
func WithoutSpans() Option {
	return func(c *config) { c.recordSpans = false }
}

// instrumentation holds the built instruments.
type instrumentation struct {
	cfg      config
	tokens   metric.Int64Histogram
	duration metric.Float64Histogram
}

// Hook returns a [skyl.Option] registering OpenTelemetry instrumentation.
//
// It records `gen_ai.client.operation.duration` and `gen_ai.client.token.usage`
// for every attempt, and a span per attempt unless [WithoutSpans] is given.
//
// Streaming is reported twice, deliberately: once at the handshake, and once
// when the stream ends, which is the only point at which streaming token usage
// exists. Duration on the second covers the whole stream.
func Hook(opts ...Option) skyl.Option {
	cfg := config{
		tracer:      otel.GetTracerProvider().Tracer(ScopeName),
		meter:       otel.GetMeterProvider().Meter(ScopeName),
		recordSpans: true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	inst := &instrumentation{cfg: cfg}

	// Instrument construction can fail if a name is invalid. That is a
	// programming error here rather than a runtime condition, and losing
	// telemetry must never take the caller's request down with it — so a
	// failure leaves the instrument nil and recording becomes a no-op.
	inst.tokens, _ = cfg.meter.Int64Histogram(
		metricTokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Number of input and output tokens used."),
	)
	inst.duration, _ = cfg.meter.Float64Histogram(
		metricDuration,
		metric.WithUnit("s"),
		metric.WithDescription("GenAI operation duration."),
	)

	return skyl.WithHook(inst.record)
}

// record turns one hook event into telemetry.
func (i *instrumentation) record(ctx context.Context, ev skyl.HookEvent) {
	// The handshake event says only that a stream started. Its duration is not
	// the operation's duration and it has no usage, so recording it as one
	// would put a misleading bar in every latency histogram. The stream_end
	// event is the one that describes the operation.
	if ev.Operation == skyl.OpStream {
		return
	}

	attrs := i.baseAttributes(ev)

	if i.duration != nil {
		i.duration.Record(ctx, ev.Duration.Seconds(), metric.WithAttributes(attrs...))
	}
	i.recordTokens(ctx, ev, attrs)

	if i.cfg.recordSpans {
		i.recordSpan(ctx, ev, attrs)
	}
}

// baseAttributes builds the attribute set both metrics share.
func (i *instrumentation) baseAttributes(ev skyl.HookEvent) []attribute.KeyValue {
	// Required by the conventions, then conditionally required.
	attrs := []attribute.KeyValue{
		attribute.String(attrOperationName, operationName(ev.Operation)),
		attribute.String(attrProviderName, ev.Provider),
	}
	if ev.Model != "" {
		attrs = append(attrs, attribute.String(attrRequestModel, ev.Model))
	}
	if ev.ResponseModel != "" {
		attrs = append(attrs, attribute.String(attrResponseModel, ev.ResponseModel))
	}
	if ev.Err != nil {
		attrs = append(attrs, attribute.String(attrErrorType, errorType(ev.Err)))
	}
	return attrs
}

// recordTokens emits the token histogram, split by direction as the
// conventions require.
func (i *instrumentation) recordTokens(ctx context.Context, ev skyl.HookEvent, base []attribute.KeyValue) {
	if i.tokens == nil {
		return
	}
	// A zero is "not reported" rather than "zero tokens" (see skyl.Usage), so
	// recording it would put a false floor in the histogram.
	if ev.Usage.InputTokens > 0 {
		attrs := append(append([]attribute.KeyValue{}, base...),
			attribute.String(attrTokenType, "input"))
		i.tokens.Record(ctx, int64(ev.Usage.InputTokens), metric.WithAttributes(attrs...))
	}
	if ev.Usage.OutputTokens > 0 {
		attrs := append(append([]attribute.KeyValue{}, base...),
			attribute.String(attrTokenType, "output"))
		i.tokens.Record(ctx, int64(ev.Usage.OutputTokens), metric.WithAttributes(attrs...))
	}
}

// recordSpan emits a span describing the attempt.
//
// The span is created and ended in one call, with an explicit start time, so
// it covers the operation that already happened. A hook cannot open a span
// around work that has finished by the time it is called.
func (i *instrumentation) recordSpan(ctx context.Context, ev skyl.HookEvent, base []attribute.KeyValue) {
	end := time.Now()
	start := end.Add(-ev.Duration)

	attrs := append([]attribute.KeyValue{}, base...)
	attrs = append(attrs, requestAttributes(ev.Request)...)

	if ev.ResponseID != "" {
		attrs = append(attrs, attribute.String(attrResponseID, ev.ResponseID))
	}
	if ev.StopReason != "" {
		// A slice even for one value: a provider may return several
		// generations, and the conventions type this as string[].
		attrs = append(attrs, attribute.StringSlice(attrFinishReasons, []string{string(ev.StopReason)}))
	}
	if ev.Usage.InputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrInputTokens, ev.Usage.InputTokens))
	}
	if ev.Usage.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrOutputTokens, ev.Usage.OutputTokens))
	}

	_, span := i.cfg.tracer.Start(ctx, spanName(ev),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(start),
		trace.WithAttributes(attrs...),
	)
	if ev.Err != nil {
		span.SetStatus(codes.Error, ev.Err.Error())
	}
	span.End(trace.WithTimestamp(end))
}

// requestAttributes records the shape of the request — never its content.
func requestAttributes(req *skyl.Request) []attribute.KeyValue {
	if req == nil {
		return nil
	}
	var attrs []attribute.KeyValue
	if req.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(attrMaxTokens, req.MaxTokens))
	}
	if req.Temperature != nil {
		attrs = append(attrs, attribute.Float64(attrTemperature, *req.Temperature))
	}
	if req.TopP != nil {
		attrs = append(attrs, attribute.Float64(attrTopP, *req.TopP))
	}
	return attrs
}

// spanName follows the convention `{operation} {model}`.
func spanName(ev skyl.HookEvent) string {
	op := operationName(ev.Operation)
	if ev.Model == "" {
		return op
	}
	return op + " " + ev.Model
}

// operationName maps skyl's operations onto the conventions' vocabulary.
//
// Both of skyl's streaming events describe a chat operation; they differ in
// when they fire, not in what was asked for, and splitting them into two
// operation names would fragment every dashboard.
func operationName(op string) string {
	switch op {
	case skyl.OpComplete, skyl.OpStream, skyl.OpStreamEnd:
		return "chat"
	case skyl.OpModels:
		return "list_models"
	default:
		return op
	}
}

// errorType renders an error as the conventions' low-cardinality error.type.
//
// It reports the skyl sentinel rather than the provider's message: a message is
// unbounded cardinality and would blow up the metric, while the sentinel is one
// of eight values and is what a dashboard actually groups by.
func errorType(err error) string {
	switch {
	case errors.Is(err, skyl.ErrAuth):
		return "auth"
	case errors.Is(err, skyl.ErrRateLimit):
		return "rate_limit"
	case errors.Is(err, skyl.ErrNotFound):
		return "not_found"
	case errors.Is(err, skyl.ErrBadRequest):
		return "bad_request"
	case errors.Is(err, skyl.ErrServer):
		return "server"
	case errors.Is(err, skyl.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, skyl.ErrRefusal):
		return "refusal"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unknown"
	}
}

// HTTPClient returns an HTTP client whose transport propagates trace context.
//
// Pass it to any adapter's WithHTTPClient option. Without it the spans this
// package produces are the end of the trace; with it, a provider that
// participates in tracing — or any proxy in between — can continue it.
//
// A nil base uses http.DefaultTransport.
func HTTPClient(base *http.Client) *http.Client {
	out := &http.Client{}
	if base != nil {
		*out = *base
	}
	inner := out.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	out.Transport = &propagatingTransport{inner: inner}
	return out
}

// propagatingTransport injects trace context into every outbound request.
//
// Hand-rolled rather than taken from otelhttp: that package would add a
// contrib dependency and would also create its own HTTP span per request,
// duplicating the GenAI span this package already emits. All that is needed
// here is header injection.
type propagatingTransport struct {
	inner http.RoundTripper
}

func (t *propagatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrip must not modify the request it is given.
	req = req.Clone(req.Context())
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	return t.inner.RoundTrip(req)
}
