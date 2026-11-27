package live

import (
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

func TestPickIsASmokeUnlessFull(t *testing.T) {
	all, err := eval.LoadDir("../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	smoke := PickScenarios(all, false, "")
	if len(smoke) != 2 || smoke[0].ID != "s01-plain-reply" || smoke[1].ID != "s15-waiting-on-command" {
		t.Fatalf("smoke: %v", smoke)
	}
	if got := PickScenarios(all, true, "s02-thread-reply, s05-handoff-failed"); len(got) != 2 {
		t.Fatalf("named: %d", len(got))
	}
	if got := PickScenarios(all, true, ""); len(got) != len(all) {
		t.Fatalf("full: %d of %d", len(got), len(all))
	}
}

func TestNewAdapterKnowsTheFiveTools(t *testing.T) {
	for _, tool := range eval.Kinds {
		if a, err := NewDockerAdapter(tool); err != nil || a == nil {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if _, err := NewDockerAdapter("emacs"); err == nil {
		t.Error("an unknown tool is refused")
	}
}
