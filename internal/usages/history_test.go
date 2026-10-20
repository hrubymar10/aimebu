package usages

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyOf(t *testing.T, r *Reader, provider, profile, window string) []HistoryPoint {
	t.Helper()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]HistoryPoint(nil), r.history[historyKey(provider, profile, window)]...)
}

func TestHistorySamplesAtMostOncePerStep(t *testing.T) {
	g := newProfileRig(t)
	now := time.Now()
	g.r.now = func() time.Time { return now }
	g.r.Refresh(context.Background())
	g.r.Refresh(context.Background())
	if got := historyOf(t, g.r, "claude-code", "local", "session"); len(got) != 1 || got[0].P != 10 {
		t.Fatalf("two refreshes inside one step must sample once: %+v", got)
	}
	now = now.Add(historyStep - time.Second)
	g.r.Refresh(context.Background())
	if got := historyOf(t, g.r, "claude-code", "local", "session"); len(got) != 1 {
		t.Fatalf("just inside the step: %+v", got)
	}
	now = now.Add(time.Second)
	g.r.Refresh(context.Background())
	if got := historyOf(t, g.r, "claude-code", "local", "session"); len(got) != 2 {
		t.Fatalf("a full step later must sample again: %+v", got)
	}
	// Stored profiles get their own series.
	if got := historyOf(t, g.r, "claude-code", "work", "session"); len(got) != 2 || got[0].P != 70 {
		t.Fatalf("stored profile series: %+v", got)
	}
}

func TestHistoryRingIsBoundedByAgeAndCount(t *testing.T) {
	g := newProfileRig(t)
	start := time.Now()
	now := start
	g.r.now = func() time.Time { return now }
	profile := Profile{ProfileName: "local", Status: "ok", Windows: []Window{{Key: "session", PercentUsed: 33}}}
	for i := 0; i < 400; i++ { // 100 h of samples
		g.r.mu.Lock()
		g.r.recordHistoryLocked("claude-code", profile)
		g.r.mu.Unlock()
		now = now.Add(historyStep)
	}
	got := historyOf(t, g.r, "claude-code", "local", "session")
	if len(got) != historyMaxPoints {
		t.Fatalf("len = %d, want the cap %d", len(got), historyMaxPoints)
	}
	if span := time.Duration(got[len(got)-1].T-got[0].T) * time.Second; span > historySpan {
		t.Fatalf("the ring spans %s, beyond %s", span, historySpan)
	}
	for i := 1; i < len(got); i++ {
		if got[i].T <= got[i-1].T {
			t.Fatal("samples are not in time order")
		}
	}
}

func TestHistoryIgnoresStaleAndFailedSnapshots(t *testing.T) {
	g := newProfileRig(t)
	g.r.mu.Lock()
	g.r.recordHistoryLocked("claude-code", Profile{ProfileName: "local", Status: "ok", Stale: true, Windows: []Window{{Key: "session", PercentUsed: 5}}})
	g.r.recordHistoryLocked("claude-code", Profile{ProfileName: "local", Status: "fetch_error", Windows: []Window{{Key: "session", PercentUsed: 5}}})
	g.r.recordHistoryLocked("claude-code", Profile{ProfileName: "local", Status: "ok", Windows: []Window{{Key: "session", PercentUsed: 250}}})
	g.r.mu.Unlock()
	got := historyOf(t, g.r, "claude-code", "local", "session")
	if len(got) != 1 || got[0].P != 100 {
		t.Fatalf("only the fresh sample (clamped) should be kept: %+v", got)
	}
}

func TestHistorySurvivesARestartAndDropsExpiredSamples(t *testing.T) {
	g := newProfileRig(t)
	now := time.Now()
	g.r.now = func() time.Time { return now }
	g.r.Refresh(context.Background())
	old := HistoryPoint{T: now.Add(-3 * historySpan).Unix(), P: 99}
	g.r.mu.Lock()
	key := historyKey("claude-code", "local", "session")
	g.r.history[key] = append([]HistoryPoint{old}, g.r.history[key]...)
	g.r.saveHistoryLocked()
	g.r.mu.Unlock()
	if info, err := os.Stat(g.r.historyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("history file: %v %v", info, err)
	}

	restarted, err := New(filepath.Dir(g.r.cachePath))
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	restarted.loadHistory()
	got := historyOf(t, restarted, "claude-code", "local", "session")
	if len(got) != 1 || got[0].P != 10 {
		t.Fatalf("restored history = %+v (the sample older than the span must be dropped)", got)
	}
}

func TestHistoryIsAttachedToOutputOnlyAndCacheStaysSmall(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	out := g.r.Snapshot()
	live := out.Providers[1].Profiles[0]
	if len(live.Windows) != 1 || len(live.Windows[0].History) != 1 {
		t.Fatalf("live window history = %+v", live.Windows)
	}
	var stored *Profile
	for i := range out.Providers[1].Profiles {
		if out.Providers[1].Profiles[i].ProfileName == "work" {
			stored = &out.Providers[1].Profiles[i]
		}
	}
	if stored == nil || len(stored.Windows[0].History) != 1 || stored.Windows[0].History[0].P != 70 {
		t.Fatalf("stored profile history = %+v", stored)
	}
	// Mutating the output must not touch the ring.
	live.Windows[0].History[0].P = -1
	if got := historyOf(t, g.r, "claude-code", "local", "session"); got[0].P != 10 {
		t.Fatal("the output shares memory with the ring")
	}
	cache, _ := os.ReadFile(g.r.cachePath)
	if strings.Contains(string(cache), `"history"`) {
		t.Fatal("history leaked into cache.json")
	}
}

func TestHistoryIsClearedWithItsProfile(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	g.r.ForgetProfile("claude-code", "work")
	if got := historyOf(t, g.r, "claude-code", "work", "session"); len(got) != 0 {
		t.Fatalf("a removed profile kept its history: %+v", got)
	}
	// A credential swap on the live account starts a new series.
	g.r.Invalidate("claude-code")
	if got := historyOf(t, g.r, "claude-code", "local", "session"); len(got) != 0 {
		t.Fatalf("history of the previous login survived a switch: %+v", got)
	}
}
