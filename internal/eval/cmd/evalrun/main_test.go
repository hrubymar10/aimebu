package main

import (
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestRunRefusesWithoutTheEvalSwitch(t *testing.T) {
	t.Setenv("AIMEBU_EVAL", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "AIMEBU_EVAL=1") {
		t.Fatalf("a live run must not start without the switch: %v", err)
	}
}

func TestPickIsASmokeUnlessFull(t *testing.T) {
	all, err := eval.LoadDir("../../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	smoke := pick(all, false, "")
	if len(smoke) != 2 || smoke[0].ID != "s01-plain-reply" || smoke[1].ID != "s15-waiting-on-command" {
		t.Fatalf("smoke: %v", smoke)
	}
	if got := pick(all, true, "s02-thread-reply, s05-handoff-failed"); len(got) != 2 {
		t.Fatalf("named: %d", len(got))
	}
	if got := pick(all, true, ""); len(got) != len(all) {
		t.Fatalf("full: %d of %d", len(got), len(all))
	}
}

func TestNewAdapterKnowsTheFiveTools(t *testing.T) {
	for _, tool := range eval.Kinds {
		if a, err := newAdapter(tool); err != nil || a == nil {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if _, err := newAdapter("emacs"); err == nil {
		t.Error("an unknown tool is refused")
	}
}
