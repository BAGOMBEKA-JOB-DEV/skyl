package testutil

import (
	"context"
	"testing"
)

// Context returns a context cancelled when the test finishes.
//
// It is what testing.T.Context does, reimplemented because that method arrived
// in Go 1.24 and the core module's floor is lower: the library itself needs
// nothing newer than Go 1.22, and there is no reason for a test helper to
// impose a floor on everyone who imports skyl (docs/adr/0001-two-module-layout.md
// applies the same reasoning to dependencies).
//
// Cancelling on test completion is not decoration. A request left running past
// its test holds a connection into the next one, which is exactly the failure
// the goroutine-leak checks in this package exist to catch.
func Context(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
