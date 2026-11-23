package supervisor

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's.
func TestMain(m *testing.M) {
	// A test that sets CommandLogger to this binary runs it as the commands' log writer.
	if len(os.Args) > 1 && os.Args[1] == "command-log" {
		os.Exit(CommandLogMain(os.Args[2:], os.Stdin))
	}
	home, cleanup := testenv.Isolate()
	// Harness discovery looks tools up on PATH: these tests ran the real claude, codex, opencode, pi and vibe, and their docker launchers, of whoever
	// ran them. A test that needs one puts its own fake on PATH.
	testenv.RestrictPATH(home)
	code := m.Run()
	cleanup()
	os.Exit(code)
}
