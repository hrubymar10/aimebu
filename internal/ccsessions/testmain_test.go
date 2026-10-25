package ccsessions

import (
	"testing"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

// Every test of this package runs with HOME and the data directories in a temporary root, never the real machine's: the scanner reads Claude's
// home, so this is also what keeps a test from reading the real one.
func TestMain(m *testing.M) { testenv.Main(m) }
