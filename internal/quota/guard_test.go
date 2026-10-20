package quota

import (
	"errors"
	"testing"
	"time"
)

func okObs(name string, session, weekly float64) Obs {
	return Obs{Name: name, Status: "ok", Session: &session, Weekly: &weekly, HasCredentials: true}
}

func TestEvaluateGuard(t *testing.T) {
	g := DefaultGuard(ToolClaude)
	for _, c := range []struct {
		name  string
		obs   Obs
		want  string
		win   string
		cfgFn func(*GuardConfig)
	}{
		{name: "below soft", obs: okObs("a", 50, 94.9), want: GuardOK, win: "weekly"},
		{name: "soft is inclusive", obs: okObs("a", 95, 10), want: GuardSoft, win: "session"},
		{name: "hard is inclusive", obs: okObs("a", 10, 99), want: GuardHard, win: "weekly"},
		{name: "fuller window decides", obs: okObs("a", 99, 97), want: GuardHard, win: "session"},
		{name: "stale is unknown", obs: Obs{Name: "a", Status: "ok", Stale: true, Weekly: ptr(99)}, want: GuardUnknown},
		{name: "error is unknown", obs: Obs{Name: "a", Status: "error", Weekly: ptr(99)}, want: GuardUnknown},
		{name: "no windows is unknown", obs: Obs{Name: "a", Status: "ok"}, want: GuardUnknown},
		{name: "no active is unknown", obs: Obs{}, want: GuardUnknown},
		{name: "disabled", obs: okObs("a", 99, 99), want: GuardOff, cfgFn: func(g *GuardConfig) { g.Enabled = false }},
		{name: "override", obs: okObs("a", 70, 70), want: GuardSoft, win: "session", cfgFn: func(g *GuardConfig) { g.Profiles["a"] = GuardLimit{Soft: 60, Hard: 80} }},
		{name: "override of another profile", obs: okObs("a", 70, 70), want: GuardOK, win: "session", cfgFn: func(g *GuardConfig) { g.Profiles["b"] = GuardLimit{Soft: 10, Hard: 20} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultGuard(ToolClaude)
			if c.cfgFn != nil {
				c.cfgFn(&cfg)
			}
			got := EvaluateGuard(cfg, c.obs)
			if got.State != c.want || (c.win != "" && got.Window != c.win) {
				t.Fatalf("got %+v, want state %s window %s", got, c.want, c.win)
			}
		})
	}
	_ = g
}

func ptr(v float64) *float64 { return &v }

func TestGuardValidate(t *testing.T) {
	known := []string{"a", "b"}
	if err := DefaultGuard(ToolClaude).Validate(known); err != nil {
		t.Fatal(err)
	}
	bad := []func(*GuardConfig){
		func(g *GuardConfig) { g.Soft = 99 },
		func(g *GuardConfig) { g.Hard = 101 },
		func(g *GuardConfig) { g.Soft = 0 },
		func(g *GuardConfig) { g.Profiles["zz"] = GuardLimit{Soft: 10, Hard: 20} },
		func(g *GuardConfig) { g.Profiles["a"] = GuardLimit{Soft: 50, Hard: 50} },
	}
	for i, f := range bad {
		g := DefaultGuard(ToolClaude)
		f(&g)
		if err := g.Validate(known); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d: err = %v", i, err)
		}
	}
}

func TestClampToSoft(t *testing.T) {
	cfg := Config{Enabled: true, Profiles: []Limit{{"a", 93, 98}, {"b", 98, 98}, {"c", 50, 60}}}
	g := DefaultGuard(ToolClaude)
	g.Profiles["b"] = GuardLimit{Soft: 80, Hard: 90}
	got := ClampToSoft(cfg, g)
	want := []Limit{{"a", 93, 95}, {"b", 80, 80}, {"c", 50, 60}}
	for i := range want {
		if got.Profiles[i] != want[i] {
			t.Fatalf("profile %d = %+v, want %+v", i, got.Profiles[i], want[i])
		}
	}
	if cfg.Profiles[0].WeeklyMax != 98 {
		t.Fatal("the input config was modified")
	}
	g.Enabled = false
	if ClampToSoft(cfg, g).Profiles[0].WeeklyMax != 98 {
		t.Fatal("a disabled guard must not clamp")
	}
}

func TestHasTarget(t *testing.T) {
	cfg := Config{Enabled: true, Profiles: []Limit{{"a", 95, 95}, {"b", 95, 95}, {"c", 95, 95}}}
	obs := func(active string, profiles ...Obs) Observation {
		return Observation{Active: active, Profiles: profiles, Ready: true}
	}
	g := DefaultGuard(ToolClaude)
	if !HasTarget(cfg, State{}, g, obs("a", okObs("a", 96, 10), okObs("b", 5, 5))) {
		t.Fatal("b is usable: there is a target")
	}
	if HasTarget(cfg, State{}, g, obs("a", okObs("a", 96, 10), okObs("b", 96, 5))) {
		t.Fatal("b is itself over the soft threshold: no target")
	}
	stale := okObs("b", 5, 5)
	stale.Stale = true
	if HasTarget(cfg, State{}, g, obs("a", okObs("a", 96, 10), stale, okObs("c", 99, 5))) {
		t.Fatal("a stale or full profile is no target")
	}
	parked := okObs("b", 5, 5)
	parked.Parked = true
	if HasTarget(cfg, State{}, g, obs("a", okObs("a", 96, 10), parked)) {
		t.Fatal("a parked profile is no target")
	}
	for name, mutate := range map[string]func(*Config){
		"switcher off": func(c *Config) { c.Enabled = false },
		"dry run":      func(c *Config) { c.DryRun = true },
		"protected":    func(c *Config) { c.Protected = []string{"a"} },
	} {
		c := cfg
		mutate(&c)
		if HasTarget(c, State{}, g, obs("a", okObs("a", 96, 10), okObs("b", 5, 5))) {
			t.Fatalf("%s: the switcher will not switch", name)
		}
	}
	if HasTarget(cfg, State{Tripped: true}, g, obs("a", okObs("a", 96, 10), okObs("b", 5, 5))) {
		t.Fatal("a tripped breaker will not switch")
	}
	if HasTarget(cfg, State{}, g, obs("", okObs("b", 5, 5))) {
		t.Fatal("no active profile: no move")
	}
}

func TestAgeObsMarksOldReadingsStale(t *testing.T) {
	now := time.Now()
	old, fresh := now.Add(-time.Hour), now.Add(-time.Minute)
	list := []Obs{{Name: "old", ObservedAt: &old}, {Name: "fresh", ObservedAt: &fresh}, {Name: "none"}}
	AgeObs(list, now, 10*time.Minute)
	if !list[0].Stale || list[1].Stale || list[2].Stale {
		t.Fatalf("stale flags = %v %v %v", list[0].Stale, list[1].Stale, list[2].Stale)
	}
	again := []Obs{{Name: "old", ObservedAt: &old}}
	AgeObs(again, now, 0)
	if again[0].Stale {
		t.Fatal("a zero max age disables the check")
	}
}

func TestTheGuardDefaultsDoNotChangeWhatTheSwitcherDoes(t *testing.T) {
	for _, tool := range Tools() {
		cfg := Default(tool, []string{"a", "b", "c"})
		clamped := ClampToSoft(cfg, DefaultGuard(tool))
		for i := range cfg.Profiles {
			if clamped.Profiles[i] != cfg.Profiles[i] {
				t.Fatalf("%s: the default guard changed %+v to %+v", tool, cfg.Profiles[i], clamped.Profiles[i])
			}
		}
		g := DefaultGuard(tool)
		if err := g.Validate([]string{"a"}); err != nil {
			t.Fatalf("%s: the default guard is invalid: %v", tool, err)
		}
	}
	if DefaultGuardSoft(ToolClaude) != 95 || DefaultGuardSoft(ToolCodex) != 98 || DefaultGuardHard != 99 {
		t.Fatal("the documented defaults are Claude 95, Codex 98 and hard 99")
	}
}
