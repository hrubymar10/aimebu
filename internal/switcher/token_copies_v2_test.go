package switcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRealManager builds a manager the way every production caller does
// (switcher.New), but over a temp tree, so the default fan-out hooks are the
// real ones.
func newRealManager(t *testing.T) (*Manager, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	m, err := New(filepath.Join(base, "config", "switcher"))
	must(t, err)
	m.SetHomeDir(home)
	return m, home
}

// F1 class: a manager built by New must carry the fan-out hooks, so a switch on
// a migrated install works from any caller (the CLI builds its manager the same
// way as the server) and writes a token-only file through the real writer.
func TestNewManagerSwitchesAMigratedInstallThroughTheRealFanOut(t *testing.T) {
	m, home := newRealManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(6 * time.Hour)
	live := writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", expiry))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", expiry)), 0o600))
	must(t, os.Remove(m.profileCredPath(ToolClaude, "a")))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "a"), []byte(fullClaude("tok-a", "ref-a", expiry)), 0o600))
	must(t, os.WriteFile(live, []byte(tokenOnlyClaude("tok-a", expiry)), 0o600))
	must(t, m.SetTokenCopies(true))

	if m.TokenCopyFanOut() == nil {
		t.Fatal("New installed no fan-out")
	}
	if _, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched {
		t.Fatalf("Switch = %v %v", switched, err)
	}
	got := readFile(t, live)
	if !strings.Contains(got, "tok-b") || strings.Contains(got, "refreshToken") {
		t.Fatalf("live file after switch = %s", got)
	}
	if stored := readFile(t, m.profileCredPath(ToolClaude, "b")); !strings.Contains(stored, "ref-b") {
		t.Fatalf("the store lost b's refresh token: %s", stored)
	}
}

// Import on a migrated install stores the live login as the profile's own
// regular file and strips the live file back to a token-only copy; it never
// recreates the symlink layout.
func TestImportOnAMigratedInstallStoresARegularFileAndStripsLive(t *testing.T) {
	m, home := newRealManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(6 * time.Hour)
	live := writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", expiry))
	must(t, m.Import(ToolClaude, "a"))
	must(t, os.Remove(m.profileCredPath(ToolClaude, "a")))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "a"), []byte(fullClaude("tok-a", "ref-a", expiry)), 0o600))
	must(t, os.WriteFile(live, []byte(tokenOnlyClaude("tok-a", expiry)), 0o600))
	must(t, m.SetTokenCopies(true))

	// A /login of another account lands a full credential in the live file.
	must(t, os.WriteFile(live, []byte(fullClaude("tok-c", "ref-c", expiry)), 0o600))
	must(t, m.Import(ToolClaude, "c"))

	info, err := os.Lstat(m.profileCredPath(ToolClaude, "c"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("imported profile is a link or missing: %v %v", info, err)
	}
	if stored := readFile(t, m.profileCredPath(ToolClaude, "c")); !strings.Contains(stored, "ref-c") {
		t.Fatalf("imported profile lost its refresh token: %s", stored)
	}
	if got := readFile(t, live); !strings.Contains(got, "tok-c") || strings.Contains(got, "refreshToken") {
		t.Fatalf("live file after import = %s", got)
	}
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "a")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the previously active profile is a link: %v %v", info, err)
	}
	if a, _ := m.Active(ToolClaude); a != "c" {
		t.Fatalf("active = %q", a)
	}
}

// Without the marker a token-only live file belongs to another install: the
// guard from the first port still refuses, and the marker turns it into the
// normal state.
func TestTokenOnlyLiveFileIsRefusedUntilTheInstallIsMigrated(t *testing.T) {
	m, home := newRealManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(6 * time.Hour)
	live := writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", expiry))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.Remove(live))
	must(t, os.WriteFile(live, []byte(tokenOnlyClaude("tok-a", expiry)), 0o600))
	if _, _, err := m.Switch(ToolClaude, "b"); err == nil || !strings.Contains(err.Error(), "token-only copy") {
		t.Fatalf("Switch on an unmigrated install = %v, want the token-only refusal", err)
	}
	must(t, m.SetTokenCopies(true))
	if el, err := m.eligibilityFor(ToolClaude); err != nil || !el.Eligible || el.TargetCopy {
		t.Fatalf("migrated eligibility = %+v %v", el, err)
	}
}
