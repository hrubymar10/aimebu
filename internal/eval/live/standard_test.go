package live

import (
	"context"
	"errors"
	"strings"
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

func TestEmbeddedScenariosAreTheDirectorys(t *testing.T) {
	dir, err := eval.LoadDir("../testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := eval.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) != len(dir) || len(embedded) == 0 {
		t.Fatalf("embedded %d, directory %d", len(embedded), len(dir))
	}
	for i := range dir {
		if embedded[i].ID != dir[i].ID {
			t.Fatalf("scenario %d: %s != %s", i, embedded[i].ID, dir[i].ID)
		}
	}
	if err := eval.ValidateAll(embedded); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaReadFnReplacesTheHTTPRead(t *testing.T) {
	want := Reading{Usage: Usage{"claude-code/main/weekly": 12}, Active: map[string]string{"claude-code": "main"}}
	got, err := QuotaSource{ReadFn: func(context.Context) (Reading, error) { return want, nil }}.Read(context.Background())
	if err != nil || got.Usage["claude-code/main/weekly"] != 12 || got.Active["claude-code"] != "main" {
		t.Fatalf("%+v %v", got, err)
	}
	boom := errors.New("boom")
	if _, err := (QuotaSource{ReadFn: func(context.Context) (Reading, error) { return Reading{}, boom }}).Fetch(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	r, err := ParseUsages(strings.NewReader(`{"providers":[{"provider_name":"codex","profiles":[{"profile_name":"p","active":true,"windows":[{"key":"weekly","percent_used":3}]}]}]}`))
	if err != nil || r.Usage["codex/p/weekly"] != 3 || r.Active["codex"] != "p" {
		t.Fatalf("%+v %v", r, err)
	}
}
