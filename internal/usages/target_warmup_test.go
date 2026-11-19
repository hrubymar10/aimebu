package usages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func warmupSnapshot(name string) (Profile, bool) {
	return Profile{ProfileName: name, Status: "ok", HasCredentials: true}, true
}

func TestWarmupSkipsActivePinnedAndLinkedProfiles(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, dir string) (refreshRunResult, error) {
		data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
		if err != nil {
			return refreshRunResult{}, err
		}
		credential, err := viewCredential("claude", data)
		if err != nil || credential.ExpiresAt == nil || !credential.ExpiresAt.After(now) {
			t.Fatal("warmup forced token expiry")
		}
		return refreshRunResult{Started: true}, nil
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "active", true, claudeFull("a", "r-a", now.Add(time.Hour)))
	h.addProfile("claude", "active-alias", false, claudeFull("a", "r-a", now.Add(time.Hour)))
	h.addProfile("claude", "pinned", false, claudeFull("b", "r-b", now.Add(time.Hour)))
	h.addProfile("claude", "pinned-alias", false, claudeFull("b", "r-b", now.Add(time.Hour)))
	linked := h.addProfile("claude", "linked", false, claudeFull("c", "r-c", now.Add(time.Hour)))
	if err := os.Remove(linked.CredPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.root, "other-tool-credentials"), linked.CredPath); err != nil {
		t.Fatal(err)
	}
	h.addProfile("claude", "idle", false, claudeFull("d", "r-d", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	h.r.cfg.WarmupPinned = func(_, name string) bool { return name == "pinned" }
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("warmup calls=%d, want one idle profile", runner.callCount())
	}
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("one-hour floor failed: calls=%d", runner.callCount())
	}
}

func TestWarmupCopiesBackUnexpectedRotationAndKeepsJournal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("new-access", "new-refresh", now.Add(2*time.Hour))}
	h := newRefreshHarness(t, runner)
	idle := h.addProfile("claude", "idle", false, claudeFull("old-access", "old-refresh", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(idle), "new-refresh") {
		t.Fatal("CLI rotation was not committed")
	}
	if len(h.fan.snapshot()) != 1 {
		t.Fatal("committed rotation was not fanned out")
	}
	if _, ok := h.r.journalFor("claude", "idle", tokenHash("claude", "old-refresh")); ok {
		t.Fatal("settled warmup left a journal entry")
	}
}

func TestWarmupScheduledMinuteAndWeeklyLimit(t *testing.T) {
	now := time.Date(2026, 10, 26, 8, 30, 0, 0, time.UTC)
	location, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	h.clock.now = now
	h.r.cfg.WarmupLocation = location
	h.addProfile("claude", "weekly-full", false, claudeFull("a", "r-a", now.Add(time.Hour)))
	h.addProfile("claude", "idle", false, claudeFull("b", "r-b", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "scheduled", "30 9 * * 1", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) {
		p, _ := warmupSnapshot(name)
		if name == "weekly-full" {
			reset := now.Add(8 * 24 * time.Hour)
			p.Windows = []Window{{Key: "weekly", PercentUsed: 100, ResetAt: &reset}}
		}
		return p, true
	}
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("scheduled calls=%d", runner.callCount())
	}
	h.clock.now = now.Add(30 * time.Second)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatal("matching minute fired twice")
	}
	h.clock.now = now.Add(7*24*time.Hour - time.Minute)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.now = now.Add(7*24*time.Hour + time.Minute)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 2 {
		t.Fatalf("missed scheduled minute did not catch up once: %d", runner.callCount())
	}
}

func TestWarmupTreatsSharedRefreshTokenAsOneAccount(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) {
				if failed {
					return refreshRunResult{Started: true}, errors.New("connection dropped after request")
				}
				return refreshRunResult{Started: true}, nil
			}}
			h := newRefreshHarness(t, runner)
			h.addProfile("claude", "first", false, claudeFull("a", "shared-refresh", now.Add(time.Hour)))
			h.addProfile("claude", "second", false, claudeFull("b", "shared-refresh", now.Add(time.Hour)))
			h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
			h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
			_ = h.r.WarmupDue(context.Background())
			if runner.callCount() != 1 {
				t.Fatalf("one account made %d requests", runner.callCount())
			}
		})
	}
}

func TestWarmupSmartSpacingCountsAccounts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "first", false, claudeFull("a", "shared-smart", now.Add(time.Hour)))
	h.addProfile("claude", "first-alias", false, claudeFull("b", "shared-smart", now.Add(time.Hour)))
	h.addProfile("claude", "second", false, claudeFull("c", "other-smart", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "smart", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	h.r.warmSmartAt = now.Add(-2 * time.Hour)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("a duplicate profile shortened smart spacing")
	}
	h.r.warmSmartAt = now.Add(-3 * time.Hour)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("smart mode made %d requests", runner.callCount())
	}
}

func TestWarmupRechecksSharedAccountInsideLock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	first := h.addProfile("claude", "first", false, claudeFull("a", "shared-lock-token", now.Add(time.Hour)))
	second := h.addProfile("claude", "second", false, claudeFull("b", "shared-lock-token", now.Add(time.Hour)))
	hash := tokenHash("claude", "shared-lock-token")
	if err := h.r.warmupProfile(context.Background(), warmupCandidate{profile: first, hash: hash}); err != nil {
		t.Fatal(err)
	}
	if err := h.r.warmupProfile(context.Background(), warmupCandidate{profile: second, hash: hash}); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("stale candidate made %d requests", runner.callCount())
	}
}

func TestWarmupRechecksTerminalAccountInsideLock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "idle", false, claudeFull("a", "terminal-token", now.Add(time.Hour)))
	hash := tokenHash("claude", "terminal-token")
	if err := h.r.stateStore().setAccount(refreshAccountEntry{Tool: "claude", Name: "idle", State: AccountNeedsReloginSoon, SpentHash: hash, Since: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.r.warmupProfile(context.Background(), warmupCandidate{profile: p, hash: hash}); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("terminal account was warmed from a stale candidate")
	}
}

func TestWarmupUnsentFailureDoesNotMarkAccountTerminal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, _ string) (refreshRunResult, error) {
		if call == 2 {
			return refreshRunResult{Started: true}, nil
		}
		return refreshRunResult{Started: true, Output: "dial tcp 127.0.0.1:443: connect: connection refused"}, errors.New("connect: connection refused")
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "idle", false, claudeFull("a", "unsent-refresh", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	// The failure goes out as the account's own alert (below); it must not also
	// fail the pass: one failure is reported once.
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatalf("an already-alerted account failure must not fail the warmup pass: %v", err)
	}
	if states := h.r.AccountStates(); len(states) != 0 {
		t.Fatalf("unsent failure changed terminal state: %+v", states)
	}
	if got := h.stateAlertKinds(); len(got) != 1 || got[0] != AccountRefreshFailed {
		t.Fatalf("unsent failure alerts=%v", got)
	}
	h.clock.now = now.Add(2 * time.Hour)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.now = now.Add(4 * time.Hour)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatalf("the re-armed account failure must not fail the warmup pass: %v", err)
	}
	if got := h.stateAlertKinds(); len(got) != 2 || got[1] != AccountRefreshFailed {
		t.Fatalf("success did not re-arm failure alert: %v", got)
	}
}

func TestWarmupHonorsSwitchAndMigrationQuietPeriods(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	claude := h.addProfile("claude", "recent", false, claudeFull("a", "quiet-claude", now.Add(time.Hour)))
	h.addProfile("codex", "migrated", false, codexFull(now.Add(time.Hour), "b", "quiet-codex", now))
	away := now.Add(-time.Minute)
	h.mu.Lock()
	h.profiles["claude"][0].SwitchedAwayAt = &away
	h.mu.Unlock()
	h.r.migratedAt = func(tool string) (time.Time, bool) { return now.Add(-time.Minute), tool == "codex" }
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("quiet profile was warmed")
	}
	h.clock.now = now.Add(codexMigrationQuiet)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 2 {
		t.Fatalf("quiet profiles did not become eligible: %d (%s)", runner.callCount(), claude.Name)
	}
}

func TestWarmupUnknownStartedFailureNeverRespendsGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(_ int, _ string) (refreshRunResult, error) {
		return refreshRunResult{Started: true}, errors.New("connection dropped after request")
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "idle", false, claudeFull("old", "same-refresh", now.Add(time.Hour)))
	h.r.cfg.WarmupSettings = func() (string, string, bool, error) { return "force", "", true, nil }
	h.r.cfg.UsageProfile = func(_, name string) (Profile, bool) { return warmupSnapshot(name) }
	if err := h.r.WarmupDue(context.Background()); err == nil {
		t.Fatal("unknown request outcome was treated as success")
	}
	h.clock.now = now.Add(2 * time.Hour)
	if err := h.r.WarmupDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("spent grant retried %d times", runner.callCount())
	}
}
