package credtarget

import (
	"fmt"
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	if err := os.Setenv("AIMEBU_URL", "http://127.0.0.1:1"); err != nil {
		fmt.Fprintln(os.Stderr, "cannot isolate AIMEBU_URL:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
