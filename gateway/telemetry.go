package gateway

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	skylotel "github.com/BAGOMBEKA-JOB-DEV/skyl/otel"
)

// Telemetry produces the gateway's metrics and the handler that exposes them.
//
// It goes through skyl/otel rather than counting requests here, so the numbers
// are the OpenTelemetry GenAI conventions — `gen_ai.client.token.usage` and
// `gen_ai.client.operation.duration` — and mean the same thing whether they
// came from this gateway or from a Go service importing skyl directly. A
// second, gateway-specific metric vocabulary would be one more thing for an
// operator to reconcile.
//
// The Prometheus exporter is what pulls prometheus/client_golang into this
// module. That is a real cost and it buys a single instrumentation path rather
// than two; an operator who runs an OTLP collector instead can leave
// [Config.MetricsEnabled] off and register their own hook.
type Telemetry struct {
	// Hook instruments every provider client.
	Hook skyl.Option

	// Handler serves the Prometheus exposition format.
	Handler http.Handler
}

// NewTelemetry builds the metrics pipeline.
//
// Spans are deliberately not recorded: a gateway sees every request in the
// estate, and one span per model call is the expensive half of instrumentation.
// A deployment that wants traces can register skylotel.Hook itself with the
// tracer provider it has already configured.
func NewTelemetry() (*Telemetry, error) {
	registry := prometheus.NewRegistry()

	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("gateway: building the metrics exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))

	return &Telemetry{
		Hook: skylotel.Hook(
			skylotel.WithMeterProvider(provider),
			skylotel.WithoutSpans(),
		),
		Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}, nil
}
