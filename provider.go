package skyl

import "context"

// Provider is the seam between skyl and a model vendor.
//
// It is deliberately four methods. Everything cross-cutting — retry, backoff,
// timeouts, validation, hooks — lives in [Client], which wraps a Provider, so
// that behaviour is written and tested once instead of once per vendor.
//
// The interface is small enough to implement outside this repository. An
// adapter in your own module is a first-class citizen: pass it to [New] and it
// inherits retry, hooks, and the gateway with no changes to skyl.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Provider interface {
	// Name returns the adapter's short identifier, for example "anthropic".
	// It appears in errors, responses, and hook events.
	Name() string

	// Complete runs a request to completion.
	//
	// It must honour ctx, populate [Response.Raw], and set
	// [Response.Provider] and [Response.Model] from the actual response.
	Complete(ctx context.Context, req *Request) (*Response, error)

	// Stream runs a request, delivering incremental events.
	//
	// The returned [Stream] is bound to ctx: cancelling ctx terminates it.
	// Callers must Close it.
	Stream(ctx context.Context, req *Request) (Stream, error)

	// Models lists what this provider currently offers.
	//
	// It queries the provider live rather than returning a compiled-in list,
	// so the answer is never stale. Providers that expose no such endpoint
	// return [ErrUnsupported].
	Models(ctx context.Context) ([]ModelInfo, error)
}
