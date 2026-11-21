package codex

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's.
func TestMain(m *testing.M) {
	if os.Getenv("AIMEBU_CODEX_WARM_FAKE_MODE") != "" {
		os.Exit(m.Run()) // the parent already provided an isolated Codex home
	}
	testenv.Main(m)
}
