package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

func codexAuth(account, access, refresh string) string {
	return `{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `","account_id":"` + account + `"}}`
}

// A running codex can write the old account's tokens back into auth.json right after a switch. The server looks at the live file at +5 s, +30 s and
// +2 min, installs the active profile again when it names another account, raises one switch_overwritten event, and never lets the old tokens
// into the stored profiles.
func TestACodexSwitchInstallsTheActiveProfileAgainWhenARunningSessionOverwritesIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f := newFixture(t)
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
	live := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	oldAccount, newAccount := codexAuth("acct-old", "old-access", "old-refresh"), codexAuth("acct-new", "new-access", "new-refresh")
	if err := os.WriteFile(live, []byte(oldAccount), 0o600); err != nil {
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
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolCodex, "new"), []byte(newAccount), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake clock: each check waits on a channel the test releases.
	var mu sync.Mutex
	var waits []chan time.Time
	var durations []time.Duration
	f.srv.switchCheckAfter = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		ch := make(chan time.Time, 1)
		waits = append(waits, ch)
		durations = append(durations, d)
		return ch
	}
	tick := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			have := len(waits)
			mu.Unlock()
			if have > n {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("check %d was never waited for", n)
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		ch := waits[n]
		mu.Unlock()
		ch <- time.Now()
	}
	resp := f.request(http.MethodPost, "/api/switcher/codex/use", map[string]string{"name": "new"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("switch status=%d", resp.StatusCode)
	}
	read := func() string {
		data, err := os.ReadFile(live)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(read(), "acct-new") {
		t.Fatalf("the switch did not install the new login: %s", read())
	}
	// +5 s: nothing was overwritten, nothing is done.
	tick(0)
	// A running session writes the old account back.
	if err := os.WriteFile(live, []byte(oldAccount), 0o600); err != nil {
		t.Fatal(err)
	}
	// +30 s: the new login is installed again.
	tick(1)
	f.srv.WaitUsageRefreshes() // the third check is cancelled here; the second has run
	if got := read(); !strings.Contains(got, "acct-new") || strings.Contains(got, "acct-old") {
		t.Fatalf("the active profile was not installed again: %s", got)
	}
	if stored, err := os.ReadFile(m.StoredCredPath(switcher.ToolCodex, "new")); err != nil || !strings.Contains(string(stored), "acct-new") || strings.Contains(string(stored), "acct-old") {
		t.Fatalf("the stored profile was overwritten by the old account: %s %v", stored, err)
	}
	mu.Lock()
	gotDurations := append([]time.Duration(nil), durations...)
	mu.Unlock()
	if len(gotDurations) < 2 || gotDurations[0] != 5*time.Second || gotDurations[1] != 25*time.Second {
		t.Fatalf("the checks were not at +5 s and +30 s: %v", gotDurations)
	}
	events := 0
	for _, event := range systemEvents(t, f) {
		if event.Kind == "switch_overwritten" && strings.Contains(event.PayloadJSON, `"tool":"codex"`) {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("switch_overwritten events = %d", events)
	}
}
