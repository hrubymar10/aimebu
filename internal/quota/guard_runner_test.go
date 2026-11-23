package quota

import (
	"context"
	"testing"
	"time"
)

type fakeSettings struct{ m map[string]string }

func (f *fakeSettings) GlobalSettingValue(_ context.Context, key string) (string, bool, error) {
	v, ok := f.m[key]
	return v, ok, nil
}
func (f *fakeSettings) SetGlobalSettingValue(_ context.Context, key, value string) error {
	f.m[key] = value
	return nil
}

type fakeHold struct{ held map[string]string }

func (f *fakeHold) SetToolHold(tool, reason string) {
	if reason == "" {
		delete(f.held, tool)
		return
	}
	f.held[tool] = reason
}

type guardHarness struct {
	g       *Guard
	src     *fakeSource
	hold    *fakeHold
	ev      *fakeEvents
	holds   int
	changes []bool
	canSwap bool
}

func newGuardHarness() *guardHarness {
	h := &guardHarness{hold: &fakeHold{held: map[string]string{}}, ev: &fakeEvents{}}
	h.src = &fakeSource{obs: map[string]Observation{}, names: map[string][]string{ToolClaude: {"a", "b"}, ToolCodex: {"a"}}}
	h.g = NewGuard(&fakeSettings{m: map[string]string{}}, h.src, h.hold, h.ev)
	h.g.OnHold = func() { h.holds++ }
	h.g.OnHoldChange = func(_ string, held bool) { h.changes = append(h.changes, held) }
	h.g.CanSwitch = func(context.Context, string, Observation, GuardConfig) bool { return h.canSwap }
	return h
}

func (h *guardHarness) observe(active Obs) {
	h.src.set(ToolClaude, Observation{Profiles: []Obs{active}, Active: active.Name, Ready: true})
	h.g.Evaluate(context.Background())
}

func TestGuardHoldsAtSoftAndReleasesBelow(t *testing.T) {
	h := newGuardHarness()
	h.observe(okObs("a", 50, 50))
	if len(h.hold.held) != 0 || len(h.ev.events) != 0 {
		t.Fatalf("below soft: held %v events %v", h.hold.held, h.ev.kinds())
	}
	h.observe(okObs("a", 96, 50))
	if h.hold.held[ToolClaude] != ReasonGuardSoft {
		t.Fatalf("held = %v", h.hold.held)
	}
	h.observe(okObs("a", 97, 50)) // still soft: no second notice
	if got := h.ev.kinds(); len(got) != 1 || got[0] != "limit_guard_soft" {
		t.Fatalf("events = %v", got)
	}
	h.observe(okObs("a", 10, 10)) // the window reset
	if len(h.hold.held) != 0 || h.ev.kinds()[1] != "limit_guard_clear" {
		t.Fatalf("held %v events %v", h.hold.held, h.ev.kinds())
	}
	v, _ := h.g.View(context.Background(), ToolClaude)
	if v.Held || v.Since != nil || v.Result.State != GuardOK {
		t.Fatalf("view = %+v", v)
	}
}

func TestGuardHardAnnouncesOnceNamesTheNextStepAndWakesTheSwitcher(t *testing.T) {
	h := newGuardHarness()
	h.observe(okObs("a", 99, 10))
	if h.hold.held[ToolClaude] != ReasonGuardHard || h.holds != 1 {
		t.Fatalf("held %v, wakeups %d", h.hold.held, h.holds)
	}
	if ev := h.ev.events[0]; ev["kind"] != "limit_guard_hard" || ev["next"] != "held" || ev["percent"] != "99" || ev["window"] != "session" {
		t.Fatalf("event = %v", ev)
	}
	h.observe(okObs("a", 100, 10))
	if len(h.ev.events) != 1 || h.holds != 1 {
		t.Fatalf("a repeat must not re-announce: %v", h.ev.kinds())
	}
	h2 := newGuardHarness()
	h2.canSwap = true
	h2.observe(okObs("a", 100, 10))
	if h2.ev.events[0]["next"] != "switching" {
		t.Fatalf("event = %v", h2.ev.events[0])
	}
	// A switch that is due but kept from happening by dry run says so, instead of a plain "held".
	h3 := newGuardHarness()
	h3.g.DryRunTarget = func(context.Context, string, Observation, GuardConfig) bool { return true }
	h3.observe(okObs("a", 100, 10))
	if h3.ev.events[0]["next"] != "dry_run" {
		t.Fatalf("event = %v", h3.ev.events[0])
	}
}

// DryRunTarget is HasTarget with dry run lifted, and only for a tool in dry run.
func TestRunnerDryRunTargetOnlyForADryRunWithATarget(t *testing.T) {
	h := newHarness(t)
	obs := Observation{Profiles: []Obs{ob("a", 1, 1), ob("b", 99, 1)}, Active: "b", Ready: true}
	g := DefaultGuard(ToolClaude)
	if h.r.DryRunTarget(context.Background(), ToolClaude, obs, g) {
		t.Fatal("a real (not dry-run) tool is not a dry-run target")
	}
	cfg := claudeCfg()
	cfg.DryRun = true
	h.save(ToolClaude, cfg)
	if !h.r.DryRunTarget(context.Background(), ToolClaude, obs, g) {
		t.Fatal("a dry run with a usable target is one")
	}
	if h.r.CanSwitch(context.Background(), ToolClaude, obs, g) {
		t.Fatal("a dry run can never switch")
	}
	obs.Profiles[0] = ob("a", 99, 1)
	if h.r.DryRunTarget(context.Background(), ToolClaude, obs, g) {
		t.Fatal("no usable target: not one")
	}
}

func TestGuardSoftSaysWhetherASwitchWillLiftTheHold(t *testing.T) {
	h := newGuardHarness()
	h.observe(okObs("a", 96, 10))
	if ev := h.ev.events[0]; ev["kind"] != "limit_guard_soft" || ev["next"] != "held" || h.holds != 1 {
		t.Fatalf("no target: %v (wakeups %d)", ev, h.holds)
	}
	h2 := newGuardHarness()
	h2.canSwap = true
	h2.observe(okObs("a", 96, 10))
	if ev := h2.ev.events[0]; ev["next"] != "switching" || h2.holds != 1 {
		t.Fatalf("with a target: %v (wakeups %d)", ev, h2.holds)
	}
}

func TestGuardIgnoresAnOldReadingAndProtectsTheImplicitLocalProfile(t *testing.T) {
	h := newGuardHarness()
	old := time.Now().Add(-time.Hour)
	stale := okObs("a", 99, 99)
	stale.ObservedAt = &old
	h.src.set(ToolClaude, Observation{Profiles: []Obs{stale}, Active: "a", Ready: true, MaxAge: 10 * time.Minute})
	h.g.Evaluate(context.Background())
	if len(h.hold.held) != 0 || len(h.ev.events) != 0 {
		t.Fatalf("a reading older than MaxAge must not hold turns: %v %v", h.hold.held, h.ev.kinds())
	}
	// A tool with no stored profile has only the implicit one; it is still protected.
	solo := okObs("local", 96, 10)
	h.src.set(ToolClaude, Observation{Solo: &solo, Ready: false})
	h.g.Evaluate(context.Background())
	if h.hold.held[ToolClaude] != ReasonGuardSoft {
		t.Fatalf("the implicit local profile at 96%% must hold new turns: %v", h.hold.held)
	}
	oldSolo := okObs("local", 96, 10)
	oldSolo.ObservedAt = &old
	h.src.set(ToolClaude, Observation{Solo: &oldSolo, MaxAge: 10 * time.Minute})
	h.g.Evaluate(context.Background())
	if len(h.hold.held) != 0 {
		t.Fatalf("an old reading of the local profile must release the hold: %v", h.hold.held)
	}
}

func TestGuardSoftThenHardIsOneNoticeEachAndHardToSoftIsQuiet(t *testing.T) {
	h := newGuardHarness()
	h.observe(okObs("a", 96, 10))
	h.observe(okObs("a", 99, 10))
	h.observe(okObs("a", 97, 10))
	if got := h.ev.kinds(); len(got) != 2 || got[0] != "limit_guard_soft" || got[1] != "limit_guard_hard" {
		t.Fatalf("events = %v", got)
	}
	if h.hold.held[ToolClaude] != ReasonGuardSoft {
		t.Fatalf("held = %v", h.hold.held)
	}
}

func TestGuardNeverHoldsOnUnclearDataAndDoesNotRepeatNoticesAcrossIt(t *testing.T) {
	h := newGuardHarness()
	h.observe(okObs("a", 96, 10))
	stale := okObs("a", 96, 10)
	stale.Stale = true
	h.observe(stale)
	if len(h.hold.held) != 0 {
		t.Fatalf("a stale reading must release the hold, got %v", h.hold.held)
	}
	if len(h.changes) != 2 || !h.changes[0] || h.changes[1] {
		t.Fatalf("hold changes after stale release = %v", h.changes)
	}
	h.observe(okObs("a", 96, 10))
	if got := h.ev.kinds(); len(got) != 1 {
		t.Fatalf("events = %v", got)
	}
	if len(h.changes) != 3 || !h.changes[2] {
		t.Fatalf("hold changes after recovery = %v", h.changes)
	}
}

func TestGuardOverridesSaveAndValidationAndDisabledReleases(t *testing.T) {
	h := newGuardHarness()
	ctx := context.Background()
	h.observe(okObs("a", 70, 10))
	cfg := DefaultGuard(ToolClaude)
	cfg.Profiles["a"] = GuardLimit{Soft: 60, Hard: 80}
	view, err := h.g.Save(ctx, ToolClaude, cfg)
	if err != nil || !view.Saved || view.Result.State != GuardSoft || h.hold.held[ToolClaude] == "" {
		t.Fatalf("view %+v err %v held %v", view, err, h.hold.held)
	}
	if _, err := h.g.Save(ctx, ToolClaude, GuardConfig{Enabled: true, Soft: 96, Hard: 95}); err == nil {
		t.Fatal("soft above hard must be refused")
	}
	if _, err := h.g.Save(ctx, "pi", cfg); err == nil {
		t.Fatal("an unknown tool must be refused")
	}
	cfg.Enabled = false
	if view, err = h.g.Save(ctx, ToolClaude, cfg); err != nil || view.Result.State != GuardOff || len(h.hold.held) != 0 {
		t.Fatalf("disabled: %+v %v held %v", view, err, h.hold.held)
	}
}

func TestTheSwitcherLeavesAProfileAtTheSoftThresholdWhenAnotherIsUsable(t *testing.T) {
	cfg := claudeCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	cfg.Profiles = []Limit{{"a", 98, 98}, {"b", 98, 98}, {"c", 98, 98}}
	// The reported case: the active profile is at 96% and the other is nearly empty. Without the guard both are under
	// their 98% limits, so the switcher would stay and the guard's hold would last until the window resets.
	h2 := newHarness(t)
	h2.save(ToolClaude, cfg)
	h2.observe("a", ob("a", 96, 1), ob("b", 5, 1), ob("c", 1, 1))
	h2.tick(time.Minute)
	if len(h2.act.calls) != 0 {
		t.Fatalf("without the guard 96%% is under the 98%% limit: %v", h2.act.calls)
	}
	h2.r.SetGuard(func(context.Context, string) GuardConfig { return DefaultGuard(ToolClaude) })
	h2.tick(time.Minute)
	if len(h2.act.calls) != 1 || h2.act.calls[0][1] != "b" {
		t.Fatalf("A at 96%% and B at 5%% must switch to B at once: %v", h2.act.calls)
	}
	// When no other profile is usable the switcher stays: the guard's hold is what protects the account.
	h3 := newHarness(t)
	h3.save(ToolClaude, cfg)
	h3.r.SetGuard(func(context.Context, string) GuardConfig { return DefaultGuard(ToolClaude) })
	h3.observe("a", ob("a", 96, 1), ob("b", 97, 1), ob("c", 98, 1))
	h3.tick(time.Minute)
	if len(h3.act.calls) != 0 {
		t.Fatalf("no usable target: %v", h3.act.calls)
	}
}

func TestASaveAndTheLoopCannotLeaveAStaleHold(t *testing.T) {
	h := newGuardHarness()
	ctx := context.Background()
	h.observe(okObs("a", 96, 10))
	if h.hold.held[ToolClaude] == "" {
		t.Fatal("the fixture should be held at 96%")
	}
	// Raising the thresholds above the reading must release the hold, whatever the loop is doing at the time.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.g.Evaluate(ctx)
		}
	}()
	cfg := DefaultGuard(ToolClaude)
	cfg.Soft, cfg.Hard = 98, 99
	for i := 0; i < 50; i++ {
		if _, err := h.g.Save(ctx, ToolClaude, cfg); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	h.g.Evaluate(ctx)
	if len(h.hold.held) != 0 {
		t.Fatalf("a stale hold survived a save: %v", h.hold.held)
	}
}
