package switcher

import (
	"os"
	"strings"
	"testing"
	"time"
)

// interruptedMigration builds the state a crashed or racing migrate-copies
// leaves: the store holds the full credential as a regular file, the live file
// is token-only and the marker is not written yet.
func interruptedMigration(t *testing.T, tool string) (m *Manager, store string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	exp := time.Now().Add(6 * time.Hour)
	var live, tokenOnly, full string
	if tool == ToolClaude {
		live = writeLive(t, home, ToolClaude, fullClaude("tok-live", "ref-live", exp))
		must(t, m.Import(ToolClaude, "main"))
		full = readFile(t, live)
		tokenOnly = strings.Replace(full, `"refreshToken":"ref-live",`, "", 1)
	} else {
		jwt := makeTestJWT("cx@example.com", exp.Unix())
		full = `{"tokens":{"access_token":"` + jwt + `","refresh_token":"ref-live","account_id":"acct"}}`
		live = writeLive(t, home, ToolCodex, full)
		must(t, m.Import(ToolCodex, "main"))
		tokenOnly = `{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"` + jwt + `","refresh_token":"","account_id":"acct"}}`
	}
	store = m.profileCredPath(tool, "main")
	must(t, os.Remove(store))
	must(t, os.WriteFile(store, []byte(full), 0o600))
	must(t, os.WriteFile(live, []byte(tokenOnly), 0o600))
	if m.TokenCopies() {
		t.Fatal("fixture: no marker expected")
	}
	return m, store
}

func assertStoreStaysRegularAndFull(t *testing.T, store string) {
	t.Helper()
	info, err := os.Lstat(store)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the store file was replaced by a link (lstat err=%v)", err)
	}
	if got := readFile(t, store); !strings.Contains(got, "ref-live") {
		t.Fatalf("the store lost its refresh token: %s", got)
	}
}

// NEW-1: the server's link loop must never turn the store file into a link to a
// token-only live file, with or without the marker.
func TestEnsureActiveLinksNeverLinksATokenOnlyLiveFile(t *testing.T) {
	for _, tool := range []string{ToolClaude, ToolCodex} {
		t.Run(tool, func(t *testing.T) {
			m, store := interruptedMigration(t, tool)
			must(t, m.EnsureActiveLinks())
			assertStoreStaysRegularAndFull(t, store)
		})
	}
}

// N1: the marker appearing between the unlocked pre-check and the lock. The
// decision is re-made under the lock (ensureActiveLink), so a tool that has been
// migrated since is left alone even if the caller saw "needs a link".
func TestEnsureActiveLinkRechecksTheMarkerUnderTheLock(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	exp := time.Now().Add(6 * time.Hour)
	live := writeLive(t, home, ToolClaude, fullClaude("tok-live", "ref-live", exp))
	must(t, m.Import(ToolClaude, "main"))
	store := m.profileCredPath(ToolClaude, "main")
	// Unmigrated and detached: a full live file and a regular stored file.
	full := readFile(t, live)
	must(t, os.Remove(store))
	must(t, os.WriteFile(store, []byte(full), 0o600))
	need, _, err := m.activeLinkNeeded(ToolClaude, "main")
	if err != nil || !need {
		t.Fatalf("fixture: an unmigrated detached profile needs a link (need=%v err=%v)", need, err)
	}
	must(t, m.SetTokenCopies(true)) // migrate-copies completes now
	lock, lerr := m.acquireLock()
	must(t, lerr)
	defer func() { _ = lock.unlock() }()
	must(t, m.ensureActiveLink(ToolClaude, "main"))
	assertStoreStaysRegularAndFull(t, store)
}

// Per tool: migrating claude must not stop the link loop for codex.
func TestEnsureActiveLinksStillLinksAnUnmigratedTool(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	jwt := makeTestJWT("cx@example.com", time.Now().Add(6*time.Hour).Unix())
	full := `{"tokens":{"access_token":"` + jwt + `","refresh_token":"ref","account_id":"acct"}}`
	live := writeLive(t, home, ToolCodex, full)
	must(t, m.Import(ToolCodex, "cx"))
	store := m.profileCredPath(ToolCodex, "cx")
	must(t, os.Remove(store)) // detached: the loop should relink it
	must(t, os.WriteFile(store, []byte(full), 0o600))
	must(t, m.markTokenCopies(ToolClaude))
	must(t, m.EnsureActiveLinks())
	info, err := os.Lstat(store)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("codex was not relinked although only claude is migrated: %v", err)
	}
	if got, _ := os.Readlink(store); got != live {
		t.Fatalf("link target = %q, want %q", got, live)
	}
}
