package usages

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

func setOn(t *testing.T, st *providers.Store, on bool) {
	t.Helper()
	if _, err := st.UpdateConfig(func(c *providers.Config) error {
		c.TokenAutoRefresh = on
		if on {
			c.TokenAutoRefreshManaged = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Before the first pass the displayed state comes from the migration markers, so a
// restart never shows "off" next to a checked toggle for a migrated tool.
func TestTokenSettingStatesAreSeededFromTheMarkers(t *testing.T) {
	ts := NewTokenSetting(providers.NewStore(t.TempDir()), nil, func(tool string) bool { return tool == "claude" }, nil)
	got := ts.States(true)
	if got["claude"].Status != "migrated" || got["codex"].Status != "pending" {
		t.Fatalf("seeded ON states = %+v", got)
	}
	if off := ts.States(false); off["claude"].Status != "off" || off["codex"].Status != "off" {
		t.Fatalf("seeded OFF states = %+v", off)
	}
}

// A toggle answers pending at once, schedules a pass right away, and the
// controller never runs twice at the same time (tick and toggle share one worker).
func TestTokenSettingToggleIsImmediateAndSingleFlight(t *testing.T) {
	st := providers.NewStore(t.TempDir())
	setOn(t, st, true)
	entered := make(chan bool, 4)
	release := make(chan struct{})
	ts := NewTokenSetting(st, func(_ context.Context, on bool) map[string]TokenAutoRefreshToolState {
		entered <- on
		<-release
		status := "off"
		if on {
			status = "migrated"
		}
		return map[string]TokenAutoRefreshToolState{"claude": {Status: status}, "codex": {Status: status}}
	}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ts.Run(ctx) }()
	defer func() { close(release); cancel(); wg.Wait() }()

	select {
	case on := <-entered:
		if !on {
			t.Fatal("the startup pass read OFF")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the startup pass did not start")
	}
	setOn(t, st, false)
	ts.Changed()
	if got := ts.States(false)["claude"].Status; got != "pending" {
		t.Fatalf("state right after the toggle = %s, want pending", got)
	}
	select {
	case <-entered:
		t.Fatal("the controller ran concurrently with itself")
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{} // the first pass ends; the queued one starts at once
	select {
	case on := <-entered:
		if on {
			t.Fatal("the queued pass missed OFF")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the toggle did not schedule an immediate pass")
	}
	release <- struct{}{}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ts.States(false)["claude"].Status == "off" {
			return
		}
	}
	t.Fatal("the state did not settle")
}

// A waiting tool is retried once a minute, not on every tick; a toggle bypasses it.
func TestTokenSettingWaitingBacksOffAndAToggleBypassesIt(t *testing.T) {
	st := providers.NewStore(t.TempDir())
	setOn(t, st, true)
	now := time.Unix(1000, 0)
	calls := 0
	ts := NewTokenSetting(st, func(context.Context, bool) map[string]TokenAutoRefreshToolState {
		calls++
		return map[string]TokenAutoRefreshToolState{"claude": {Status: "waiting", Reason: "busy"}, "codex": {Status: "off"}}
	}, nil, nil)
	ts.SetClock(func() time.Time { return now })
	ctx := context.Background()
	ts.reconcile(ctx)
	for i := 0; i < 20; i++ {
		now = now.Add(2 * time.Second)
		ts.reconcile(ctx)
	}
	if calls != 1 {
		t.Fatalf("a waiting tool was retried %d times within 40 s", calls)
	}
	now = now.Add(30 * time.Second)
	ts.reconcile(ctx)
	if calls != 2 {
		t.Fatalf("calls = %d after the backoff, want 2", calls)
	}
	setOn(t, st, false)
	ts.Changed()
	ts.reconcile(ctx)
	if calls != 3 {
		t.Fatalf("a toggle did not bypass the backoff: %d calls", calls)
	}
}

// Off and never owned: a manual migrate-copies is left alone, the controller is
// not even asked.
func TestTokenSettingOffAndUnmanagedDoesNothing(t *testing.T) {
	st := providers.NewStore(t.TempDir())
	called := false
	ts := NewTokenSetting(st, func(context.Context, bool) map[string]TokenAutoRefreshToolState {
		called = true
		return nil
	}, nil, nil)
	ts.reconcile(context.Background())
	if called {
		t.Fatal("the controller ran for an unmanaged OFF setting")
	}
	if got := ts.States(false)["claude"].Status; got != "off" {
		t.Fatalf("state = %s", got)
	}
}
