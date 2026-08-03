// Package testutil holds helpers shared by skyl's tests.
//
// It lives under internal/ so it is never part of skyl's public API, and it
// depends only on the standard library — the core module's zero-dependency
// guarantee applies to test helpers too (docs/rules.md §4.1), which rules out
// pulling in a leak-detection library.
package testutil

import (
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// leakSettleTimeout bounds how long we wait for goroutines to wind down.
//
// A goroutine that is genuinely exiting does so in microseconds; this budget
// exists only so a slow CI runner doesn't produce a false positive.
const leakSettleTimeout = 2 * time.Second

// CheckNoGoroutineLeaks fails the test if it finishes with more goroutines
// running than it started with.
//
// Call it at the top of a test:
//
//	defer testutil.CheckNoGoroutineLeaks(t)()
//
// Note the trailing `()`: the call returns the check, and `defer` runs it when
// the test ends.
//
// This matters more than it looks. A stream that leaks its reader goroutine
// leaks one per request, which never shows up in a unit test's wall clock and
// only surfaces as unbounded memory growth in production at 3am. `-race` does
// not catch it — a leaked goroutine that is merely blocked is not a data race.
func CheckNoGoroutineLeaks(t *testing.T) func() {
	t.Helper()

	// Let anything already winding down from a previous test finish first, so
	// we measure this test rather than its predecessor.
	before := stableGoroutineCount()

	return func() {
		t.Helper()

		deadline := time.Now().Add(leakSettleTimeout)
		var after int
		for {
			after = runtime.NumGoroutine()
			if after <= before {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			// Yield rather than spin: the goroutines we are waiting on need
			// scheduler time to reach their exit.
			time.Sleep(5 * time.Millisecond)
		}

		t.Errorf("goroutine leak: %d before, %d after (waited %v)\n\n%s",
			before, after, leakSettleTimeout, interestingStacks())
	}
}

// stableGoroutineCount waits for the goroutine count to stop moving, then
// returns it.
//
// Without this, a baseline taken while an earlier test's HTTP connections are
// still closing reads low, and the next test is blamed for the difference.
func stableGoroutineCount() int {
	deadline := time.Now().Add(leakSettleTimeout)
	prev := runtime.NumGoroutine()

	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

// interestingStacks renders the goroutine dump with the runtime's own
// bookkeeping removed, so the report shows the leak rather than the scheduler.
func interestingStacks() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	var keep []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if stack == "" || isRuntimeNoise(stack) {
			continue
		}
		keep = append(keep, stack)
	}
	sort.Strings(keep)

	const maxReported = 10
	if len(keep) > maxReported {
		keep = keep[:maxReported]
	}
	return strings.Join(keep, "\n\n")
}

// runtimeNoise names goroutines that are always present in a test binary and
// are never the leak being hunted.
//
// Deliberately narrow. A broad marker such as "runtime.gopark" would match
// almost every parked goroutine — including the leaked one — and quietly turn
// the report empty, which is worse than noisy.
var runtimeNoise = []string{
	"testing.tRunner",       // the test harness running each test
	"testing.(*M).Run",      // the test binary's main
	"os/signal.signal_recv", // signal handler, started by the runtime
}

func isRuntimeNoise(stack string) bool {
	for _, marker := range runtimeNoise {
		if strings.Contains(stack, marker) {
			return true
		}
	}
	return false
}
