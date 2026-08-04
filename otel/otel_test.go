package otel_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	skylotel "github.com/BAGOMBEKA-JOB-DEV/skyl/otel"
)

// These tests assert the exact attribute keys and metric names the
// OpenTelemetry GenAI semantic conventions specify. Asserting merely that "a
// span was produced" would pass with every key misspelled, which is precisely
// the failure that makes an instrumentation library useless — the data arrives,
// and no dashboard matches it.

// harness wires an in-memory tracer and meter to a skyl client.
type harness struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	client *skyl.Client
}

func newHarness(t *testing.T, p skyl.Provider, opts ...skylotel.Option) *harness {
	t.Helper()

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	opts = append([]skylotel.Option{
		skylotel.WithTracerProvider(tp),
		skylotel.WithMeterProvider(mp),
	}, opts...)

	return &harness{
		spans:  spans,
		reader: reader,
		client: skyl.New(p, skylotel.Hook(opts...)),
	}
}

// metrics collects the recorded metrics by name.
func (h *harness) metrics(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrOf(t *testing.T, set attribute.Set, key string) attribute.Value {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		t.Errorf("attribute %q is absent; present: %v", key, set.Encoded(attribute.DefaultEncoder()))
	}
	return v
}

func request() *skyl.Request {
	temp, topP := 0.3, 0.9
	return &skyl.Request{
		Model:       "gpt-5.6",
		MaxTokens:   256,
		Temperature: &temp,
		TopP:        &topP,
		Messages:    []skyl.Message{skyl.UserText("What is the capital of France?")},
	}
}

func TestCompleteProducesConventionalSpan(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeProvider{resp: &skyl.Response{
		ID:         "chatcmpl-1",
		Provider:   "openai",
		Model:      "gpt-5.6-0613",
		StopReason: skyl.StopEndTurn,
		Usage:      skyl.Usage{InputTokens: 12, OutputTokens: 5},
	}})

	if _, err := h.client.Complete(context.Background(), request()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]

	// The conventions name the span `{operation} {model}`.
	if span.Name() != "chat gpt-5.6" {
		t.Errorf("span name = %q, want %q", span.Name(), "chat gpt-5.6")
	}
	if span.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", span.SpanKind())
	}

	set := attribute.NewSet(span.Attributes()...)
	for _, tc := range []struct {
		key  string
		want any
	}{
		{"gen_ai.operation.name", "chat"},
		{"gen_ai.provider.name", "openai"},
		{"gen_ai.request.model", "gpt-5.6"},
		{"gen_ai.response.model", "gpt-5.6-0613"},
		{"gen_ai.response.id", "chatcmpl-1"},
		{"gen_ai.usage.input_tokens", int64(12)},
		{"gen_ai.usage.output_tokens", int64(5)},
		{"gen_ai.request.max_tokens", int64(256)},
		{"gen_ai.request.temperature", 0.3},
		{"gen_ai.request.top_p", 0.9},
	} {
		got := attrOf(t, set, tc.key).AsInterface()
		if got != tc.want {
			t.Errorf("%s = %v (%T), want %v (%T)", tc.key, got, got, tc.want, tc.want)
		}
	}

	// finish_reasons is a slice even when there is one, per the conventions.
	reasons := attrOf(t, set, "gen_ai.response.finish_reasons").AsStringSlice()
	if len(reasons) != 1 || reasons[0] != string(skyl.StopEndTurn) {
		t.Errorf("finish_reasons = %v, want [%s]", reasons, skyl.StopEndTurn)
	}

	// A successful call must not carry error.type.
	if _, ok := set.Value("error.type"); ok {
		t.Error("error.type is present on a successful call")
	}
}

func TestMetricsFollowTheConventions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeProvider{resp: &skyl.Response{
		Provider: "openai", Model: "gpt-5.6-0613",
		Usage: skyl.Usage{InputTokens: 12, OutputTokens: 5},
	}})

	if _, err := h.client.Complete(context.Background(), request()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	got := h.metrics(t)

	duration, ok := got["gen_ai.client.operation.duration"]
	if !ok {
		t.Fatalf("gen_ai.client.operation.duration absent; got %v", keys(got))
	}
	if duration.Unit != "s" {
		t.Errorf("duration unit = %q, want %q", duration.Unit, "s")
	}
	if _, isHist := duration.Data.(metricdata.Histogram[float64]); !isHist {
		t.Errorf("duration is %T, want a float64 histogram", duration.Data)
	}

	tokens, ok := got["gen_ai.client.token.usage"]
	if !ok {
		t.Fatalf("gen_ai.client.token.usage absent; got %v", keys(got))
	}
	if tokens.Unit != "{token}" {
		t.Errorf("token unit = %q, want %q", tokens.Unit, "{token}")
	}
	hist, isHist := tokens.Data.(metricdata.Histogram[int64])
	if !isHist {
		t.Fatalf("token usage is %T, want an int64 histogram", tokens.Data)
	}

	// One data point per direction, tagged with gen_ai.token.type.
	byType := map[string]int64{}
	for _, dp := range hist.DataPoints {
		v, ok := dp.Attributes.Value("gen_ai.token.type")
		if !ok {
			t.Error("a token data point has no gen_ai.token.type")
			continue
		}
		byType[v.AsString()] = dp.Sum
	}
	if byType["input"] != 12 {
		t.Errorf("input tokens = %d, want 12", byType["input"])
	}
	if byType["output"] != 5 {
		t.Errorf("output tokens = %d, want 5", byType["output"])
	}
}

// A failure must be classified into a bounded set. Recording the provider's
// message would make error.type unbounded and blow up the metric's cardinality.
func TestErrorsAreClassifiedNotStringified(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"rate limit", skyl.NewError("openai", 429, skyl.ErrRateLimit, "slow down, user 12345", nil), "rate_limit"},
		{"auth", skyl.NewError("openai", 401, skyl.ErrAuth, "bad key", nil), "auth"},
		{"server", skyl.NewError("openai", 500, skyl.ErrServer, "", nil), "server"},
		{"unclassified", errors.New("something else"), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, &fakeProvider{err: tc.err})
			// No retries: one attempt, one span.
			h.client = skyl.New(&fakeProvider{err: tc.err},
				skylotel.Hook(skylotel.WithTracerProvider(sdktrace.NewTracerProvider(
					sdktrace.WithSpanProcessor(h.spans)))),
				skyl.WithMaxRetries(0))

			if _, err := h.client.Complete(context.Background(), request()); err == nil {
				t.Fatal("Complete() error = nil, want the injected failure")
			}

			spans := h.spans.Ended()
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			set := attribute.NewSet(spans[0].Attributes()...)
			if got := attrOf(t, set, "error.type").AsString(); got != tc.want {
				t.Errorf("error.type = %q, want %q", got, tc.want)
			}
			if spans[0].Status().Code != codes.Error {
				t.Errorf("span status = %v, want Error", spans[0].Status().Code)
			}
			// The provider's message must not become the attribute.
			if got := attrOf(t, set, "error.type").AsString(); len(got) > 20 {
				t.Errorf("error.type = %q, suspiciously long for a classification", got)
			}
		})
	}
}

// The handshake event describes a stream that has not produced a token yet.
// Recording it as an operation would put a meaningless bar in every latency
// histogram and double-count every stream.
func TestStreamRecordsOnceAtTheEnd(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeProvider{stream: &scriptedStream{events: []skyl.StreamEvent{
		{Type: skyl.EventTextDelta, Text: "Paris"},
		{
			Type:       skyl.EventDone,
			Usage:      &skyl.Usage{InputTokens: 7, OutputTokens: 2},
			StopReason: skyl.StopEndTurn,
		},
	}}})

	s, err := h.client.Stream(context.Background(), request())
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for s.Next() { //nolint:revive // draining is the point
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	spans := h.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want exactly 1 — the handshake must not produce one", len(spans))
	}

	set := attribute.NewSet(spans[0].Attributes()...)
	// Streaming usage is the whole point: before the terminal hook event
	// existed, this was unreportable.
	if got := attrOf(t, set, "gen_ai.usage.output_tokens").AsInt64(); got != 2 {
		t.Errorf("output tokens = %d, want 2 from the stream's terminal event", got)
	}
	if got := attrOf(t, set, "gen_ai.operation.name").AsString(); got != "chat" {
		t.Errorf("operation = %q, want chat for a streaming call too", got)
	}
}

func TestWithoutSpansRecordsMetricsOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeProvider{resp: &skyl.Response{
		Provider: "openai", Usage: skyl.Usage{InputTokens: 3, OutputTokens: 1},
	}}, skylotel.WithoutSpans())

	if _, err := h.client.Complete(context.Background(), request()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got := len(h.spans.Ended()); got != 0 {
		t.Errorf("got %d spans with WithoutSpans, want 0", got)
	}
	if _, ok := h.metrics(t)["gen_ai.client.operation.duration"]; !ok {
		t.Error("metrics were suppressed too; WithoutSpans must leave them alone")
	}
}

// Prompts must not reach a span. The hook is handed the whole request, so this
// is a decision the package makes rather than a limitation it has.
func TestPromptContentIsNeverRecorded(t *testing.T) {
	t.Parallel()

	const secret = "my-confidential-prompt-text"
	h := newHarness(t, &fakeProvider{resp: &skyl.Response{Provider: "openai"}})

	req := request()
	req.System = secret
	req.Messages = []skyl.Message{skyl.UserText(secret)}

	if _, err := h.client.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	for _, span := range h.spans.Ended() {
		for _, attr := range span.Attributes() {
			if v := attr.Value.String(); v == secret {
				t.Errorf("attribute %s carries the prompt", attr.Key)
			}
		}
	}
}

// Without propagation the spans this package emits are the end of the trace.
func TestHTTPClientInjectsTraceContext(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// The global propagator is what an application configures; set it here so
	// the test reflects a real setup.
	prev := otelPropagator()
	t.Cleanup(func() { setOtelPropagator(prev) })
	setOtelPropagator(propagation.TraceContext{})

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "outer")
	defer span.End()

	client := skylotel.HTTPClient(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if got == "" {
		t.Error("traceparent was not injected; the trace stops at skyl")
	}
}

func keys(m map[string]metricdata.Metrics) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
