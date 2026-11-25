package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectAndRemoveJournals(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "supervisor")
	legacy := filepath.Join(dir, LegacyJournalName)
	side := filepath.Join(InstanceJournalDir(dir), "x.sqlite")
	for _, path := range []string{legacy, side} {
		j, err := OpenJournal(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if path == side {
			if _, err := j.RecordStart(ctx, StartRecord{TurnID: "t1", AgentID: "a1", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
		_ = j.Close()
	}
	got := InstanceJournals(dir)
	if len(got) != 2 || got[0] != legacy || got[1] != side || !IsLegacyJournal(legacy) || IsLegacyJournal(side) {
		t.Fatalf("journals: %v", got)
	}
	if st, err := InspectJournal(ctx, legacy); err != nil || st.Owes || st.ID == "" {
		t.Fatalf("empty journal: %+v %v", st, err)
	}
	if st, err := InspectJournal(ctx, side); err != nil || !st.Owes {
		t.Fatalf("journal with an open turn: %+v %v", st, err)
	}
	if removed, err := RemoveJournal(ctx, side); err != nil || removed {
		t.Fatalf("a journal that owes work was removed: %v %v", removed, err)
	}
	if removed, err := RemoveJournal(ctx, legacy); err != nil || !removed {
		t.Fatalf("an empty journal was kept: %v %v", removed, err)
	}
	for _, suffix := range []string{"", ".lock"} {
		if _, err := os.Stat(legacy + suffix); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", legacy+suffix)
		}
	}
	held, err := OpenJournal(ctx, side)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := InspectJournal(ctx, side); err == nil {
		t.Fatal("a held journal was inspected")
	}
	if removed, err := RemoveJournal(ctx, side); err != nil || removed {
		t.Fatalf("a held journal was removed: %v %v", removed, err)
	}
	a, _ := NewInstanceJournalPath(dir, time.Now())
	b, _ := NewInstanceJournalPath(dir, time.Now())
	if a == b || filepath.Dir(a) != InstanceJournalDir(dir) {
		t.Fatalf("names: %s %s", a, b)
	}
}
