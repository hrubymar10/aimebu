package providers

import (
	"testing"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's.
func TestMain(m *testing.M) { testenv.Main(m) }
