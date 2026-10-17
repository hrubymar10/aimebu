package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
)

func enabledManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	return m, home
}

func TestImportLoginCreatesARegularProfile(t *testing.T) {
	m, _ := enabledManager(t)
	full := futureClaude("login-token")
	must(t, m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "login-1", Create: true, Full: []byte(full), Email: "kim@example.test", SetActive: true}))
	path := m.StoredCredPath(ToolClaude, "login-1")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored credential must be a regular 0600 file: %v %v", info, err)
	}
	if got := readFile(t, path); got != full {
		t.Fatalf("stored bytes differ: %q", got)
	}
	if got := m.ProfileEmail(ToolClaude, "login-1"); got != "kim@example.test" {
		t.Fatalf("email = %q", got)
	}
	if active, _ := m.Active(ToolClaude); active != "login-1" {
		t.Fatalf("active = %q", active)
	}
	if err := m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "login-1", Create: true, Full: []byte(full)}); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("creating twice = %v", err)
	}
}

func TestImportLoginReplaceKeepsABackupAndIsIdempotent(t *testing.T) {
	m, _ := enabledManager(t)
	must(t, m.Add(ToolClaude, "main"))
	old := futureClaude("old-token")
	must(t, os.WriteFile(m.StoredCredPath(ToolClaude, "main"), []byte(old), 0o600))
	fresh := futureClaude("new-token")
	in := credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Full: []byte(fresh), Email: "alice@example.test"}
	must(t, m.ImportLogin(in))
	if got := readFile(t, m.StoredCredPath(ToolClaude, "main")); got != fresh {
		t.Fatalf("not replaced: %q", got)
	}
	backups, err := m.profileBackups(ToolClaude, "main")
	if err != nil || len(backups) != 1 || readFile(t, backups[0].path) != old {
		t.Fatalf("the replaced credential must be backed up: %v %v", backups, err)
	}
	must(t, m.ImportLogin(in))
	again, _ := m.profileBackups(ToolClaude, "main")
	if len(again) != 1 {
		t.Fatalf("a repeated import minted another backup: %d", len(again))
	}
}

func TestImportLoginRefusesToWriteThroughALink(t *testing.T) {
	m, home := enabledManager(t)
	must(t, m.Add(ToolClaude, "main"))
	original := futureClaude("live-token")
	live := writeLive(t, home, ToolClaude, original)
	must(t, os.Symlink(live, m.StoredCredPath(ToolClaude, "main")))
	err := m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Full: []byte(futureClaude("x"))})
	if !errors.Is(err, ErrLinkedProfile) {
		t.Fatalf("link must be refused, got %v", err)
	}
	if got := readFile(t, live); got != original {
		t.Fatalf("the live file was written through the link: %q", got)
	}
}

func TestImportLoginReplaceNeedsAnExistingProfile(t *testing.T) {
	m, _ := enabledManager(t)
	err := m.ImportLogin(credtarget.LoginImport{Tool: ToolCodex, Profile: "ghost", Full: []byte(futureCodex("a@example.test"))})
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("missing profile = %v", err)
	}
	if err := m.ImportLogin(credtarget.LoginImport{Tool: ToolCodex, Profile: "../escape", Create: true, Full: []byte("{}")}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("path escape = %v", err)
	}
	if err := m.ImportLogin(credtarget.LoginImport{Tool: "vibe", Profile: "x", Create: true, Full: []byte("{}")}); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if err := m.ImportLogin(credtarget.LoginImport{Tool: ToolCodex, Profile: "empty", Create: true, Full: []byte("  ")}); err == nil {
		t.Fatal("empty credential accepted")
	}
}

func TestImportLoginSetActiveStampsTheLeavingProfile(t *testing.T) {
	m, _ := enabledManager(t)
	must(t, m.Add(ToolCodex, "work"))
	must(t, os.WriteFile(m.StoredCredPath(ToolCodex, "work"), []byte(futureCodex("kim@example.test")), 0o600))
	state, _ := m.loadState()
	state.Active[ToolCodex] = "work"
	must(t, m.saveState(state))
	must(t, m.ImportLogin(credtarget.LoginImport{Tool: ToolCodex, Profile: "home", Create: true, Full: []byte(futureCodex("pat@example.test")), SetActive: true}))
	if active, _ := m.Active(ToolCodex); active != "home" {
		t.Fatalf("active = %q", active)
	}
	if _, ok := m.loadSwitchedAway()[switchedAwayKey(ToolCodex, "work")]; !ok {
		t.Fatal("the profile that stopped being active is not stamped as switched away")
	}
}

func TestProfileNamesAndStoredCredential(t *testing.T) {
	m, _ := enabledManager(t)
	if names, err := m.ProfileNames(ToolClaude); err != nil || len(names) != 0 {
		t.Fatalf("no profiles yet: %v %v", names, err)
	}
	must(t, m.Add(ToolClaude, "b"))
	must(t, m.Add(ToolClaude, "a"))
	names, err := m.ProfileNames(ToolClaude)
	if err != nil || strings.Join(names, ",") != "a,b" {
		t.Fatalf("names = %v %v", names, err)
	}
	if _, err := m.StoredCredential(ToolClaude, "a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty profile has no credential: %v", err)
	}
	if _, err := m.StoredCredential(ToolClaude, "../x"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("path escape = %v", err)
	}
}

func TestStoredCredentialRefusesALinkedProfile(t *testing.T) {
	m, home := enabledManager(t)
	must(t, m.Add(ToolClaude, "main"))
	live := writeLive(t, home, ToolClaude, futureClaude("live-token"))
	must(t, os.Symlink(live, m.StoredCredPath(ToolClaude, "main")))
	if _, err := m.StoredCredential(ToolClaude, "main"); !errors.Is(err, ErrLinkedProfile) {
		t.Fatalf("a linked profile's bytes are the live file's, not the store's: %v", err)
	}
}

func TestSaveLoginRescueReusesAndPrunes(t *testing.T) {
	m, _ := enabledManager(t)
	at := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	first, err := m.SaveLoginRescue(ToolClaude, []byte("one"), at)
	if err != nil || readFile(t, first) != "one" {
		t.Fatalf("first rescue: %v %v", first, err)
	}
	if info, _ := os.Stat(first); info.Mode().Perm() != 0o600 {
		t.Fatalf("rescue mode = %v", info.Mode().Perm())
	}
	same, err := m.SaveLoginRescue(ToolClaude, []byte("one"), at.Add(time.Hour))
	if err != nil || same != first {
		t.Fatalf("an identical credential must reuse its rescue: %v %v", same, err)
	}
	for i := 0; i < 8; i++ {
		_, err := m.SaveLoginRescue(ToolClaude, []byte{byte('a' + i)}, at.Add(time.Duration(i+2)*time.Minute))
		must(t, err)
	}
	entries, _ := os.ReadDir(filepath.Join(m.backupsRoot(), ToolClaude))
	if len(entries) != maxLoginRescues {
		t.Fatalf("rescues kept = %d, want %d", len(entries), maxLoginRescues)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the oldest rescue should have been pruned")
	}
	// A login rescue is never mistaken for a profile backup.
	if backups, _ := m.profileBackups(ToolClaude, ""); len(backups) != 0 {
		t.Fatalf("rescues leaked into profile backups: %v", backups)
	}
}
