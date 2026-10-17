package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// The real startup sequence of Run (startTokenCopies, then usages.Manager.Start):
// somebody must start the refresher, and only once. A due profile is refreshed
// exactly once, a second Run is a no-op, and nothing keeps spending afterwards.
func TestTokenCopiesStartupStartsTheRefresherExactlyOnce(t *testing.T) {
	m, home := newSwitcherTestEnv(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(20 * time.Minute) // inside the 60 min claude lead: due
	if err := os.WriteFile(live, []byte(fullClaudeCreds("acc-A0", "ref-A0", soon)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	// migrate-copies (a separate CLI process in real life) wires its own hooks.
	reg := credtarget.NewRegistry(t.TempDir(), home)
	reg.LivePath = m.LiveCredPath
	migFan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: m.Active}
	m.SetTokenCopyHooks(switcher.TokenCopyHooks{FanOutActive: migFan.FanOutActive, ReplaceActive: migFan.FanOutActiveReplacing})
	if _, err := m.MigrateToTokenCopies(switcher.MigrateOptions{
		Tools:        []string{switcher.ToolClaude},
		RunningCodex: func() ([]string, string, error) { return nil, "test", nil },
		Now:          func() time.Time { return time.Now().Add(-time.Hour) }, // the 15-minute guard is not under test
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s, _ := setupTestServer(t)
	um := usages.NewManager(usages.NewStoreAt(t.TempDir()), usages.EmptyRegistry())
	sr := switcherRoutes{sm: m}
	um.SetProfileLister(func(tool string) ([]usages.ProfileInfo, error) {
		profiles, err := m.List()
		if err != nil {
			return nil, err
		}
		var out []usages.ProfileInfo
		for _, p := range profiles {
			if p.Tool == tool {
				out = append(out, usages.ProfileInfo{Tool: p.Tool, Name: p.Name, Active: p.Active, CredPath: sr.profileCredPath(p), Email: p.Email, HasCredentials: p.HasCreds, ExpiresAt: p.ExpiresAt, SwitchedAwayAt: p.SwitchedAwayAt})
			}
		}
		return out, nil
	})
	um.EnableClaudeAutoRefresh(m.WithLock, m.Root())

	cli := &fakeCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	tr := s.startTokenCopies(ctx, &wg, m, um, t.TempDir(), func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetExecutor("codex", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	if tr == nil {
		t.Fatal("startTokenCopies did not build a refresher")
	}
	um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for cli.totalCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := cli.totalCalls(); got != 1 {
		t.Fatalf("after startup the due profile was handed to the CLI %d times, want 1 (nobody started the refresher, or it ran twice)", got)
	}
	// A second owner asking to run the loop is a no-op, and nothing keeps spending.
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second Run must return immediately")
	}
	time.Sleep(1500 * time.Millisecond)
	cli.assertNothingSpentTwice(t, "after startup")
	if got := cli.totalCalls(); got != 1 {
		t.Fatalf("calls = %d after a second Run and a pause, want 1", got)
	}
	if data, err := os.ReadFile(live); err != nil || containsRefresh(string(data)) {
		t.Fatalf("live file after the startup refresh: %v %s", err, data)
	}
}

func containsRefresh(s string) bool { return claudeRefreshInFile.MatchString(s) }
