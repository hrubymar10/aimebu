package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// codexSwitchEnv is a server whose switcher holds the Codex profiles old (live, account acct-old) and new (stored, account acct-new). The timed
// overwrite checks never fire on their own: their clock is a channel nobody sends on.
func codexSwitchEnv(t *testing.T) (f *fixture, m *switcher.Manager, live string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f = newFixture(t)
	m, err := switcher.New(filepath.Join(root, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	f.srv.Switcher = m
	f.srv.Usages, err = usages.New(filepath.Join(root, "usage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.switchCheckAfter = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	t.Cleanup(f.srv.WaitUsageRefreshes)
	live = filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(codexAuth("acct-old", "old-access", "old-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/switcher", map[string]bool{"enabled": true}},
		{"POST", "/api/switcher/codex/import", map[string]string{"name": "old"}},
		{"POST", "/api/switcher/codex/profiles", map[string]string{"name": "new"}},
	} {
		resp := f.request(step.method, step.path, step.body, true)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d", step.method, step.path, resp.StatusCode)
		}
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolCodex, "new"), []byte(codexAuth("acct-new", "new-access", "new-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, m, live
}

func readLive(t *testing.T, live string) string {
	t.Helper()
	data, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func overwrittenEvents(t *testing.T, f *fixture) int {
	t.Helper()
	n := 0
	for _, event := range systemEvents(t, f) {
		if event.Kind == "switch_overwritten" && strings.Contains(event.PayloadJSON, `"tool":"codex"`) {
			n++
		}
	}
	return n
}

// A running codex writes the old account back before the check has looked at the live file even once. The account the check expects comes from
// what the switch installed, not from that first read, so the overwrite is repaired at once and raises the event.
func TestACodexOverwriteBeforeTheFirstCheckIsRepaired(t *testing.T) {
	f, m, live := codexSwitchEnv(t)
	if _, changed, err := m.SwitchGuarded(switcher.ToolCodex, "new", switcher.SwitchGuard{ExpectedActive: "old"}); err != nil || !changed {
		t.Fatalf("switch changed=%t err=%v", changed, err)
	}
	// Between the swap and the server's follow-up, the old account comes back.
	if err := os.WriteFile(live, []byte(codexAuth("acct-old", "old-access-2", "old-refresh-2")), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.accountSwitched(context.Background(), switcher.ToolCodex)
	if got := readLive(t, live); !strings.Contains(got, "acct-new") || strings.Contains(got, "acct-old") {
		t.Fatalf("the old account written back before the first check stayed installed: %s", got)
	}
	if n := overwrittenEvents(t, f); n != 1 {
		t.Fatalf("switch_overwritten events = %d", n)
	}
}

// After the timed checks, the live Codex login is looked at again on every usage refresh and before a Codex turn starts on the host. Each check
// compares the account id; a match keeps the newest tokens to put back, an old profile's account is replaced, and a login no stored profile holds
// (made outside the switcher) is left alone. One switch raises one event however many checks repair it.
func TestTheCodexOverwriteCheckRunsOnUsageRefreshesAndBeforeTurns(t *testing.T) {
	f, _, live := codexSwitchEnv(t)
	resp := f.request("POST", "/api/switcher/codex/use", map[string]string{"name": "new"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("switch status=%d", resp.StatusCode)
	}
	// codex rotates the new account's tokens; the check keeps the newest ones.
	rotated := codexAuth("acct-new", "new-access-2", "new-refresh-2")
	if err := os.WriteFile(live, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.checkCodexOverwrite()

	// A usage refresh: the check runs in the background (the usage reader holds its lock while it signals).
	if err := os.WriteFile(live, []byte(codexAuth("acct-old", "old-access", "old-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.usagesChanged()
	deadline := time.Now().Add(5 * time.Second)
	for readLive(t, live) != rotated {
		if time.Now().After(deadline) {
			t.Fatalf("a usage refresh did not put the newest login back: %s", readLive(t, live))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Before a Codex turn on the host.
	if err := os.WriteFile(live, []byte(codexAuth("acct-old", "old-access", "old-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.checkCodexOverwrite()
	if got := readLive(t, live); got != rotated {
		t.Fatalf("the check before a turn did not put the login back: %s", got)
	}

	// A login of an account no profile holds is not ours to undo.
	foreign := codexAuth("acct-other", "other-access", "other-refresh")
	if err := os.WriteFile(live, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.checkCodexOverwrite()
	if got := readLive(t, live); got != foreign {
		t.Fatalf("a login made outside the switcher was replaced: %s", got)
	}
	if n := overwrittenEvents(t, f); n != 1 {
		t.Fatalf("switch_overwritten events = %d", n)
	}
}
