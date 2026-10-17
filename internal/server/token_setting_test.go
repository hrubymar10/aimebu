package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

type tokenSettingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *tokenSettingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *tokenSettingClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestTokenSettingMigratesWaitsRetriesAndUnmigrates(t *testing.T) {
	e := newE2EEnv(t)
	clock := &tokenSettingClock{now: time.Now()}
	e.um.SetClock(clock)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "harness-docker-ctrl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.setLive(switcher.ToolClaude, fullClaudeCreds("access", "refresh", time.Now().Add(10*time.Minute)))
	if err := e.m.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := e.um.UpdateSettings(context.Background(), 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := setupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.startTokenCopies(ctx, &wg, e.m, e.um, t.TempDir(), func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", e.cli)
		tr.SetExecutor("codex", e.cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	e.um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()
	state := func() usages.TokenAutoRefreshToolState {
		resp, err := e.um.Snapshot(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Settings.TokenAutoRefreshState[switcher.ToolClaude]
	}
	wait := func(want string) usages.TokenAutoRefreshToolState {
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			got := state()
			if got.Status == want {
				return got
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("state = %+v, want %s", state(), want)
		return usages.TokenAutoRefreshToolState{}
	}
	if got := wait("waiting"); !strings.Contains(got.Reason, "15m") {
		t.Fatalf("waiting reason = %q", got.Reason)
	}
	if e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("migrated despite refusal")
	}
	e.setLive(switcher.ToolClaude, fullClaudeCreds("access", "refresh", time.Now().Add(6*time.Hour)))
	clock.Advance(time.Minute)
	wait("migrated")
	if !e.m.TokenCopiesFor(switcher.ToolClaude) || strings.Contains(readFileString(t, e.liveClaude), `"refreshToken"`) {
		t.Fatal("migration did not strip live")
	}
	on = false
	if _, err := e.um.UpdateSettings(ctx, 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wait("off")
	if e.m.TokenCopiesFor(switcher.ToolClaude) || !strings.Contains(readFileString(t, e.liveClaude), `"refreshToken"`) {
		t.Fatal("unmigration did not restore full live credential")
	}
	info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "main"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("store was not relinked: %v %v", info, err)
	}
	e.cli.assertNothingSpentTwice(t, "setting round trip")
}

func TestTokenSettingCodexWaitingReasonInSnapshot(t *testing.T) {
	e := newE2EEnv(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "harness-docker-ctrl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e.setLive(switcher.ToolCodex, fullCodexCreds("cx0", "ref-cx0", time.Now().Add(6*time.Hour), time.Now()))
	if err := e.m.Import(switcher.ToolCodex, "main"); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := e.um.UpdateSettings(context.Background(), 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := setupTestServer(t)
	s.tokenSettingRunningCodex = func() ([]string, string, error) {
		return []string{"pid 4242", "container abc: 7"}, "host and docker", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.startTokenCopies(ctx, &wg, e.m, e.um, t.TempDir())
	e.um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()
	for deadline := time.Now().Add(18 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		resp, err := e.um.Snapshot(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		state := resp.Settings.TokenAutoRefreshState[switcher.ToolCodex]
		if state.Status == "waiting" {
			want := "waiting for running codex sessions to end (2 found); retries automatically"
			if state.Reason != want {
				t.Fatalf("settings state = %+v, want reason %q", state, want)
			}
			return
		}
	}
	t.Fatal("Codex never reached waiting state")
}

func TestTokenSettingOffStaysOffWhenLinkRepairFails(t *testing.T) {
	e := newE2EEnv(t)
	// A corrupt switcher state makes EnsureActiveLinks fail, but no tool has
	// migrated, so the setting must continue to report OFF.
	if err := os.WriteFile(filepath.Join(e.swRoot, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := setupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.startTokenCopies(ctx, &wg, e.m, e.um, t.TempDir())
	e.um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()
	// A changed setting gives a pending state until the controller has run.
	on := true
	if _, err := e.um.UpdateSettings(ctx, 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	sawWaiting := false
	for time.Now().Before(deadline) {
		resp, err := e.um.Snapshot(ctx, "")
		if err == nil && resp.Settings.TokenAutoRefreshState[switcher.ToolClaude].Status == "waiting" {
			sawWaiting = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawWaiting {
		t.Fatal("ON controller did not run")
	}
	on = false
	if _, err := e.um.UpdateSettings(ctx, 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err := e.um.Snapshot(ctx, "")
		if err == nil && resp.Settings.TokenAutoRefreshState[switcher.ToolClaude].Status == "off" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("unmigrated tool did not return to off after link-repair error")
}

func TestLegacyOnSettingCanUnmigrateAfterUpgrade(t *testing.T) {
	e := newE2EEnv(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "harness-docker-ctrl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	mcp := `{"account":{"accessToken":"mcp-token"}}`
	full := fullClaudeCreds("access", "refresh", time.Now().Add(6*time.Hour))
	e.setLive(switcher.ToolClaude, strings.TrimSuffix(full, "}")+`,"mcpOAuth":`+mcp+`}`)
	if err := e.m.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.usageStore.ConfigPath(), []byte(`{"claude_auto_refresh":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := e.usageStore.LoadConfig()
	if err != nil || !cfg.ClaudeAutoRefresh || !cfg.TokenAutoRefreshManaged {
		t.Fatalf("legacy setting did not carry ownership: %+v, %v", cfg, err)
	}
	s, _ := setupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.startTokenCopies(ctx, &wg, e.m, e.um, t.TempDir())
	e.um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()
	wait := func(want string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := e.um.Snapshot(ctx, "")
			if err == nil && resp.Settings.TokenAutoRefreshState[switcher.ToolClaude].Status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("Claude state did not become %s", want)
	}
	wait("migrated")
	off := false
	if _, err := e.um.UpdateSettings(ctx, 0, "", nil, false, &off, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wait("off")
	if e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("legacy ON migration marker survived OFF")
	}
	live := readFileString(t, e.liveClaude)
	if !strings.Contains(live, `"refreshToken":"refresh"`) || !strings.Contains(live, `"mcpOAuth":`+mcp) {
		t.Fatalf("full live credential or MCP OAuth missing after OFF: %s", live)
	}
}
