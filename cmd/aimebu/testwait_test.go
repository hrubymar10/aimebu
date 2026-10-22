package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime/pprof"
	"testing"
	"time"
)

// testHTTP is the client for requests a test sends to a server it started. http.DefaultClient has no deadline, so a server that accepted a
// connection and never answered would block a test until go test's own timeout, with nothing printed. Every wait on a request ends here.
var testHTTP = &http.Client{Timeout: 30 * time.Second}

// dumpGoroutines prints every goroutine of the test binary to stderr, the way a SIGQUIT would, so a wait that gave up shows what it was
// waiting for.
func dumpGoroutines(reason string) {
	fmt.Fprintf(os.Stderr, "--- %s; goroutines of the test binary:\n", reason)
	_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
}

// receive waits for a value for at most limit. A wait on a channel without a deadline (the server's exit, a hook that never fires) is how
// a test hangs without a word: on a slow or stuck machine it just sits at 0% CPU. A timeout dumps the goroutines and fails the test.
func receive[T any](t testing.TB, what string, ch <-chan T, limit time.Duration) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(limit):
		dumpGoroutines(fmt.Sprintf("%s: nothing within %s", what, limit))
		t.Fatalf("%s: nothing within %s (the goroutines are on stderr)", what, limit)
		panic("unreachable")
	}
}

// Generous: these bounds are for a hang, not for a slow machine. The slowest start of a server takes about 40 s under the race detector
// with the whole suite running, and shutting one down takes seconds.
const (
	serverStartLimit = 4 * time.Minute
	serverStopLimit  = 2 * time.Minute
)
