package switcher

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Listing is read-only: an unreadable file counts as no recorded time and stays
// exactly where it is.
func TestSwitchedAwayCorruptFileIsIgnoredWhenReading(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, os.MkdirAll(m.root, 0o700))
	must(t, os.WriteFile(m.switchedAwayPath(), []byte("{not json"), 0o600))
	if got := m.loadSwitchedAway(); len(got) != 0 {
		t.Fatalf("corrupt file produced entries: %v", got)
	}
	if _, err := m.List(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, m.switchedAwayPath()); got != "{not json" {
		t.Fatalf("a read changed the file: %q", got)
	}
}

// A file that cannot be parsed also holds other profiles' records (v1's too).
// A writer sets it aside instead of replacing it with a one-entry file.
func TestSwitchedAwayCorruptFileIsSetAsideBeforeWriting(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	must(t, m.Add(ToolClaude, "profile-b"))
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "profile-b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte(futureClaude("tok-b")), 0o600))
	bad := "{\n  \"codex/other\": \"2026-10-15T08:30:00Z\",\n  \"claude/x\": \"not a time\"\n}\n"
	must(t, os.WriteFile(m.switchedAwayPath(), []byte(bad), 0o600))

	if _, _, err := m.Switch(ToolClaude, "profile-b"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, m.switchedAwayPath()+".corrupt"); got != bad {
		t.Fatalf("the unreadable file was not preserved: %q", got)
	}
	if data := readFile(t, m.switchedAwayPath()); !strings.Contains(data, `"claude/profile-a"`) {
		t.Fatalf("the new record lacks the switch-away stamp: %s", data)
	}
	// A second unreadable file does not overwrite the first copy.
	must(t, os.WriteFile(m.switchedAwayPath(), []byte("{again"), 0o600))
	if _, _, err := m.Switch(ToolClaude, "profile-a"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, m.switchedAwayPath()+".corrupt"); got != bad {
		t.Fatalf("the first preserved copy was replaced: %q", got)
	}
	if got := readFile(t, m.switchedAwayPath()+".corrupt.1"); got != "{again" {
		t.Fatalf("the second unreadable file was not preserved: %q", got)
	}
}

func switchedAwayOf(t *testing.T, m *Manager, tool, name string) *time.Time {
	t.Helper()
	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Tool == tool && p.Name == name {
			return p.SwitchedAwayAt
		}
	}
	t.Fatalf("profile %s/%s missing", tool, name)
	return nil
}

// Import moves the active pointer to the adopted login, so the profile that was
// active until then stops being active.
func TestSwitchedAwayRecordedByImport(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	if switchedAwayOf(t, m, ToolClaude, "profile-a") != nil {
		t.Fatal("the first import has no previous profile to stamp")
	}
	writeLive(t, home, ToolClaude, futureClaude("tok-b"))
	must(t, m.Import(ToolClaude, "profile-b"))
	if switchedAwayOf(t, m, ToolClaude, "profile-a") == nil {
		t.Fatal("importing a second login did not stamp the profile it replaced")
	}
	if switchedAwayOf(t, m, ToolClaude, "profile-b") != nil {
		t.Fatal("the newly active profile reports a switch-away time")
	}
}

// Switching to an empty profile takes a different path in Switch (state first,
// then the live file is removed).
func TestSwitchedAwayRecordedBySwitchToEmptyProfile(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	must(t, m.Add(ToolClaude, "empty"))
	if _, _, err := m.Switch(ToolClaude, "empty"); err != nil {
		t.Fatal(err)
	}
	if switchedAwayOf(t, m, ToolClaude, "profile-a") == nil {
		t.Fatal("switching to an empty profile did not stamp the outgoing one")
	}
}

// The active profile has no entry of its own. A stale one left under the old or
// the new name must not be inherited when the active profile is renamed.
func TestSwitchedAwayRenameOfActiveProfileDropsStaleEntries(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	stale := "{\n  \"claude/profile-a\": \"2026-10-16T12:00:00Z\",\n  \"claude/renamed\": \"2026-10-16T12:00:00Z\",\n  \"codex/keep\": \"2026-10-15T08:30:00Z\"\n}\n"
	must(t, os.WriteFile(m.switchedAwayPath(), []byte(stale), 0o600))
	must(t, m.Rename(ToolClaude, "profile-a", "renamed"))
	data := readFile(t, m.switchedAwayPath())
	if strings.Contains(data, "claude/profile-a") || strings.Contains(data, "claude/renamed") {
		t.Fatalf("stale entries survived the rename of the active profile: %s", data)
	}
	if !strings.Contains(data, "codex/keep") {
		t.Fatalf("an unrelated entry was lost: %s", data)
	}
}

// Renaming an inactive profile onto a name that has a stale entry replaces it.
func TestSwitchedAwayRenameOntoStaleNameReplacesIt(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	must(t, m.Add(ToolClaude, "profile-b"))
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "profile-b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte(futureClaude("tok-b")), 0o600))
	if _, _, err := m.Switch(ToolClaude, "profile-b"); err != nil { // stamps profile-a
		t.Fatal(err)
	}
	// profile-b is active; profile-a is inactive with a fresh stamp and "ghost"
	// has an old entry from a profile that no longer exists.
	entries := m.loadSwitchedAway()
	entries["claude/ghost"] = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	m.saveSwitchedAway(entries)
	want := switchedAwayOf(t, m, ToolClaude, "profile-a")
	must(t, m.Rename(ToolClaude, "profile-a", "ghost"))
	got := switchedAwayOf(t, m, ToolClaude, "ghost")
	if got == nil || want == nil || !got.Equal(*want) {
		t.Fatalf("renamed profile switched-away = %v, want %v (not the stale 2020 entry)", got, want)
	}
}

// A JSON "null" parses without error but yields a nil map; recording into it
// would panic in the middle of a switch.
func TestSwitchedAwayNullFileDoesNotPanic(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	must(t, m.Add(ToolClaude, "profile-b"))
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "profile-b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte(futureClaude("tok-b")), 0o600))
	must(t, os.WriteFile(m.switchedAwayPath(), []byte("null\n"), 0o600))
	if got := m.loadSwitchedAway(); got == nil {
		t.Fatal("loadSwitchedAway returned a nil map for a null file")
	}
	if _, _, err := m.Switch(ToolClaude, "profile-b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, m.switchedAwayPath()), `"claude/profile-a"`) {
		t.Fatal("the switch-away stamp was not recorded over a null file")
	}
}
