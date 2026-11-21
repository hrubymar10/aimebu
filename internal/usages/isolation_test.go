package usages

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) {
	root, cleanup := testenv.Isolate()
	// An empty PATH keeps the Claude user-agent probe from running a real claude.
	if err := os.Setenv("PATH", root); err != nil {
		panic(err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
