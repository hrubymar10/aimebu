package credtarget

import (
	"os"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
