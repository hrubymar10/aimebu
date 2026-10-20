package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/switcher"
)

func TestTokenCopyMigrationReasonForRunningCodex(t *testing.T) {
	err := &switcher.CodexRunningError{
		Found:   []string{"pid 4242", "container abc: 7"},
		Checked: "host processes; 2 running docker container(s)",
	}
	got := tokenCopyMigrationReason(err)
	want := "waiting for running codex sessions to end (2 found); retries automatically"
	if got != want || strings.Contains(got, "4242") || strings.Contains(got, "abc") {
		t.Fatalf("settings reason = %q, want %q without process details", got, want)
	}
	if other := tokenCopyMigrationReason(errors.New("another refusal")); other != "another refusal" {
		t.Fatalf("other refusal = %q", other)
	}
}

func TestCodexBlockLogStateChangesOnlyOnBlockingEdges(t *testing.T) {
	state := codexBlockLogState{}
	first := &switcher.CodexRunningError{Found: []string{"pid 4242", "container abc: 7"}}
	reordered := &switcher.CodexRunningError{Found: []string{"container abc: 7", "pid 4242"}}
	changed := &switcher.CodexRunningError{Found: []string{"pid 4242", "pid 99"}}
	if !state.changed("codex", first) || state.changed("codex", first) || state.changed("codex", reordered) {
		t.Fatal("unchanged blocking set logged more than once")
	}
	if !state.changed("codex", changed) {
		t.Fatal("changed blocking set was not logged")
	}
	if state.changed("codex", errors.New("token near expiry")) || !state.changed("codex", first) {
		t.Fatal("a block after a different refusal was not logged")
	}
	if state.changed("codex", nil) || !state.changed("codex", first) {
		t.Fatal("a block after recovery was not logged")
	}
}
