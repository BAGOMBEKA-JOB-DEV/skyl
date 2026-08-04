package otel_test

import (
	"context"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// otelPropagator / setOtelPropagator wrap the global propagator so a test can
// restore it, keeping the package's own API surface free of test hooks.
func otelPropagator() propagation.TextMapPropagator { return otel.GetTextMapPropagator() }
func setOtelPropagator(p propagation.TextMapPropagator) {
	otel.SetTextMapPropagator(p)
}

// fakeProvider returns a canned response, stream, or error.
type fakeProvider struct {
	resp   *skyl.Response
	err    error
	stream skyl.Stream
}

func (fakeProvider) Name() string { return "openai" }

func (f *fakeProvider) Complete(context.Context, *skyl.Request) (*skyl.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeProvider) Stream(context.Context, *skyl.Request) (skyl.Stream, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

func (f *fakeProvider) Models(context.Context) ([]skyl.ModelInfo, error) {
	return nil, f.err
}

// scriptedStream replays a fixed event list.
type scriptedStream struct {
	events []skyl.StreamEvent
	i      int
}

func (s *scriptedStream) Next() bool {
	if s.i >= len(s.events) {
		return false
	}
	s.i++
	return true
}

func (s *scriptedStream) Event() skyl.StreamEvent { return s.events[s.i-1] }
func (s *scriptedStream) Err() error              { return nil }
func (s *scriptedStream) Close() error            { return nil }
