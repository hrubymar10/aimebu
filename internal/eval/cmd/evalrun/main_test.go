package main

import (
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestRunRefusesWithoutTheEvalSwitch(t *testing.T) {
	t.Setenv("AIMEBU_EVAL", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "AIMEBU_EVAL=1") {
		t.Fatalf("a live run must not start without the switch: %v", err)
	}
}
