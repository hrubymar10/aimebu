package usages

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTokenCopySettingsSeedAndImmediateSerialReconcile(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	m.SetTokenCopySeed(func(tool string) bool { return tool == "claude" })
	on := true
	if _, err := m.UpdateSettings(context.Background(), 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	before, err := m.Snapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Settings.TokenAutoRefreshState["claude"].Status; got != "migrated" {
		t.Fatalf("startup claude status = %s, want migrated", got)
	}
	if got := before.Settings.TokenAutoRefreshState["codex"].Status; got != "pending" {
		t.Fatalf("startup codex status = %s, want pending", got)
	}
	entered := make(chan bool, 4)
	release := make(chan struct{})
	m.SetTokenCopyController(func(_ context.Context, on bool) map[string]TokenAutoRefreshToolState {
		entered <- on
		<-release
		status := "off"
		if on {
			status = "migrated"
		}
		return map[string]TokenAutoRefreshToolState{"claude": {Status: status}, "codex": {Status: status}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	m.Start(ctx, &wg)
	defer func() { close(release); cancel(); wg.Wait() }()
	select {
	case got := <-entered:
		if !got {
			t.Fatal("startup reconcile read OFF")
		}
	case <-time.After(time.Second):
		t.Fatal("startup reconcile did not start immediately")
	}
	on = false
	info, err := m.UpdateSettings(ctx, 0, "", nil, false, &on, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.TokenAutoRefreshState["claude"].Status; got != "pending" {
		t.Fatalf("POST state = %s, want pending", got)
	}
	select {
	case <-entered:
		t.Fatal("controller ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case got := <-entered:
		if got {
			t.Fatal("queued reconcile missed OFF")
		}
	case <-time.After(time.Second):
		t.Fatal("toggle did not schedule immediate reconcile")
	}
	release <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err := m.Snapshot(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if resp.Settings.TokenAutoRefreshState["claude"].Status == "off" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("toggle state did not settle")
}

func TestTokenCopyWaitingBackoffAndToggleBypass(t *testing.T) {
	m := NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	clock := &fakeClock{now: time.Unix(1000, 0)}
	m.SetClock(clock)
	count := 0
	m.SetTokenCopyController(func(_ context.Context, on bool) map[string]TokenAutoRefreshToolState {
		count++
		return map[string]TokenAutoRefreshToolState{
			"claude": {Status: "waiting", Reason: "busy"},
			"codex":  {Status: "off"},
		}
	})
	on := true
	if _, err := m.UpdateSettings(context.Background(), 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	m.reconcileTokenCopies(context.Background())
	for i := 0; i < 20; i++ {
		clock.now = clock.now.Add(2 * time.Second)
		m.reconcileTokenCopies(context.Background())
	}
	if count != 1 {
		t.Fatalf("waiting controller called %d times within 40 seconds", count)
	}
	clock.now = clock.now.Add(20 * time.Second)
	m.reconcileTokenCopies(context.Background())
	if count != 2 {
		t.Fatalf("waiting controller called %d times after 60 seconds", count)
	}
	on = false
	if _, err := m.UpdateSettings(context.Background(), 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	m.reconcileTokenCopies(context.Background())
	if count != 3 {
		t.Fatalf("toggle did not bypass waiting backoff: %d calls", count)
	}
}
