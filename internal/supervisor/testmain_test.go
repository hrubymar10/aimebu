package supervisor

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's.
func TestMain(m *testing.M) {
	home, cleanup := testenv.Isolate()
	// Harness discovery looks tools up on PATH: these tests ran the real claude, codex, opencode and pi, and their docker launchers, of whoever
	// ran them. A test that needs one puts its own fake on PATH.
	testenv.RestrictPATH(home)
	code := m.Run()
	cleanup()
	os.Exit(code)
}
