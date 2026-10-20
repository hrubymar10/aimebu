package quota

import (
	"testing"
	"time"
)

// Every name here is synthetic: a, b, c, p. The numbers are the rules of the
// watchdog this replaces (Claude 95/95; Codex 93/98 then 98/98).

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

// ob is a healthy, settled, credentialed profile with a far token expiry.
func ob(name string, session, weekly float64) Obs {
	exp := t0.Add(6 * time.Hour)
	o := Obs{Name: name, Status: "ok", Weekly: f(weekly), OKFor: 10 * time.Minute, HasCredentials: true, TokenExpiresAt: &exp}
	if session >= 0 {
		o.Session = f(session)
	}
	return o
}

func claudeCfg() Config {
	c := Default(ToolClaude, []string{"a", "b", "c"})
	c.Enabled, c.DryRun = true, false
	return c
}

func codexCfg() Config {
	c := Default(ToolCodex, []string{"a", "b"})
	c.Enabled, c.DryRun = true, false
	return c
}

func decide(cfg Config, state State, active string, migrated bool, obs ...Obs) Result {
	tool := ToolClaude
	return Decide(Input{Tool: tool, Config: cfg, State: state, Profiles: obs, Active: active, Now: t0, Migrated: migrated})
}

func want(t *testing.T, r Result, action, reason, from, to string) {
	t.Helper()
	d := r.Decision
	got := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if d.Action != action || d.Reason != reason || got(d.From) != from || got(d.To) != to {
		t.Fatalf("decision = %s %q %q->%q, want %s %q %q->%q", d.Action, d.Reason, got(d.From), got(d.To), action, reason, from, to)
	}
	if action == ActionHold && r.State.Hold != reason {
		t.Fatalf("state.hold = %q, want %q", r.State.Hold, reason)
	}
	if action != ActionHold && r.State.Hold != "" {
		t.Fatalf("state.hold = %q on a non-hold decision", r.State.Hold)
	}
}

func TestDefaultsFollowTodaysThresholdsByPosition(t *testing.T) {
	c := Default(ToolClaude, []string{"x", "y", "z"})
	for _, l := range c.Profiles {
		if l.SessionMax != 95 || l.WeeklyMax != 95 {
			t.Fatalf("claude default %+v", l)
		}
	}
	x := Default(ToolCodex, []string{"x", "y", "z"})
	if x.Profiles[0].SessionMax != 93 || x.Profiles[0].WeeklyMax != 98 || x.Profiles[1].SessionMax != 98 || x.Profiles[2].WeeklyMax != 98 {
		t.Fatalf("codex default %+v", x.Profiles)
	}
	if x.Enabled || !x.DryRun || x.WarmupSeconds != 60 || x.MinDwellMinutes != 5 {
		t.Fatalf("a default must be off, dry-run, 60 s, 5 min: %+v", x)
	}
}

func TestSwitchesToTheFirstUsableProfile(t *testing.T) {
	// Active b is fine but a (higher priority) is usable again.
	r := decide(claudeCfg(), State{}, "b", true, ob("a", 10, 10), ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
	// Active a is over its weekly limit: the next usable one wins.
	r = decide(claudeCfg(), State{}, "a", true, ob("a", 10, 96), ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonOverLimit, "a", "b")
	// And it skips an exhausted b for c.
	r = decide(claudeCfg(), State{}, "a", true, ob("a", 10, 96), ob("b", 96, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonOverLimit, "a", "c")
}

func TestAnActiveProfileThatIsNotInTheOrderIsLeftWithItsOwnReason(t *testing.T) {
	cfg := claudeCfg()
	cfg.Profiles = cfg.Profiles[:2] // a, b are listed; c is not
	r := decide(cfg, State{}, "c", true, ob("a", 10, 10), ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonActiveNotListed, "c", "a")
	// A listed active profile that is over its limit keeps the over-limit reason.
	r = decide(cfg, State{}, "a", true, ob("a", 10, 96), ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonOverLimit, "a", "b")
}

func TestLimitsAreStrict(t *testing.T) {
	for _, tc := range []struct {
		name            string
		session, weekly float64
		usable          bool
	}{
		{"94/94", 94, 94, true}, {"95 session", 95, 10, false}, {"95 weekly", 10, 95, false}, {"96", 96, 96, false}, {"0/0", 0, 0, true},
	} {
		r := decide(claudeCfg(), State{}, "b", true, ob("a", tc.session, tc.weekly), ob("b", 10, 10), ob("c", 0, 0))
		if tc.usable {
			want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
		} else {
			want(t, r, ActionStay, ReasonActiveIsBest, "b", "b")
		}
	}
}

func TestANullSessionIsNotLimitingButANullWeeklyIsUnusable(t *testing.T) {
	noSession := ob("a", -1, 10) // the provider reported no session window
	r := decide(claudeCfg(), State{}, "b", true, noSession, ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
	noWeekly := ob("a", 10, 10)
	noWeekly.Weekly = nil
	r = decide(claudeCfg(), State{}, "b", true, noWeekly, ob("b", 10, 10), ob("c", 0, 0))
	want(t, r, ActionStay, ReasonActiveIsBest, "b", "b")
}

func TestNothingUsableStaysPut(t *testing.T) {
	r := decide(claudeCfg(), State{}, "b", true, ob("a", 99, 99), ob("b", 99, 99), ob("c", 99, 99))
	want(t, r, ActionStay, ReasonNothingUsable, "b", "b")
}

func TestCodexFallsBackToTheSecondEntryAndKeepsTheFirstsTighterLimit(t *testing.T) {
	cfg := codexCfg()
	tool := func(active string, a, b Obs) Result {
		return Decide(Input{Tool: ToolCodex, Config: cfg, Profiles: []Obs{a, b}, Active: active, Now: t0, Migrated: true})
	}
	// a session 92 < 93: usable; 93 is not.
	want(t, tool("b", ob("a", 92, 50), ob("b", 10, 10)), ActionSwitch, ReasonHigherUsable, "b", "a")
	want(t, tool("a", ob("a", 93, 50), ob("b", 10, 10)), ActionSwitch, ReasonOverLimit, "a", "b")
	// a weekly 98 is not < 98.
	want(t, tool("a", ob("a", 10, 98), ob("b", 10, 10)), ActionSwitch, ReasonOverLimit, "a", "b")
	// b has the looser 98 session limit.
	want(t, tool("a", ob("a", 93, 50), ob("b", 97, 10)), ActionSwitch, ReasonOverLimit, "a", "b")
	// Both exhausted: stay.
	want(t, tool("b", ob("a", 99, 99), ob("b", 99, 99)), ActionStay, ReasonNothingUsable, "b", "b")
}

func TestHoldsInTheirOrder(t *testing.T) {
	// Disabled.
	cfg := claudeCfg()
	cfg.Enabled = false
	want(t, decide(cfg, State{}, "a", true, ob("a", 1, 1), ob("b", 1, 1)), ActionIdle, ReasonOff, "", "")
	// Too few listed profiles that exist.
	want(t, decide(claudeCfg(), State{}, "a", true, ob("a", 1, 1)), ActionIdle, ReasonTooFew, "", "")
	// Tripped beats everything below it.
	st := State{Tripped: true}
	want(t, decide(claudeCfg(), st, "b", true, ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1)), ActionHold, ReasonTripped, "b", "")
	// No active profile.
	want(t, decide(claudeCfg(), State{}, "", true, ob("a", 1, 1), ob("b", 1, 1)), ActionHold, ReasonNoActive, "", "")
	// Protected active.
	prot := claudeCfg()
	prot.Profiles = prot.Profiles[:2]
	prot.Protected = []string{"c"}
	want(t, decide(prot, State{}, "c", true, ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1)), ActionHold, ReasonProtected, "c", "")
	// Unclear active: not ok, stale, unknown to the snapshot.
	bad := ob("b", 1, 1)
	bad.Status = "refreshing"
	want(t, decide(claudeCfg(), State{}, "b", true, ob("a", 1, 1), bad, ob("c", 1, 1)), ActionHold, ReasonActiveNotOK, "b", "")
	stale := ob("b", 1, 1)
	stale.Stale = true
	want(t, decide(claudeCfg(), State{}, "b", true, ob("a", 1, 1), stale, ob("c", 1, 1)), ActionHold, ReasonActiveNotOK, "b", "")
	want(t, decide(claudeCfg(), State{}, "ghost", true, ob("a", 1, 1), ob("b", 1, 1)), ActionHold, ReasonActiveNotOK, "ghost", "")
}

func TestWarmupIsHeldByAnyOkProfileNotOnlyTheCandidate(t *testing.T) {
	young := ob("c", 1, 1)
	young.OKFor = 59 * time.Second
	r := decide(claudeCfg(), State{}, "b", true, ob("a", 99, 99), ob("b", 99, 99), young)
	want(t, r, ActionHold, ReasonWarmingUp, "b", "")
	young.OKFor = 60 * time.Second
	r = decide(claudeCfg(), State{}, "b", true, ob("a", 99, 99), ob("b", 99, 99), young)
	want(t, r, ActionSwitch, ReasonOverLimit, "b", "c")
	// A profile that is not ok does not hold the warm-up (it has no ok streak to wait for).
	notOK := ob("c", 1, 1)
	notOK.Status, notOK.OKFor = "refreshing", 0
	r = decide(claudeCfg(), State{}, "b", true, ob("a", 1, 1), ob("b", 1, 1), notOK)
	want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
	// A zero warm-up turns the hold off.
	cfg := claudeCfg()
	cfg.WarmupSeconds = 0
	young.OKFor = 0
	want(t, decide(cfg, State{}, "b", true, ob("a", 99, 99), ob("b", 99, 99), young), ActionSwitch, ReasonOverLimit, "b", "c")
}

func TestAProfileThatIsNotSettledIsNotATarget(t *testing.T) {
	cfg := claudeCfg()
	cfg.WarmupSeconds = 0
	noCreds := ob("a", 1, 1)
	noCreds.HasCredentials = false
	r := decide(cfg, State{}, "c", true, noCreds, ob("b", 99, 99), ob("c", 99, 99))
	want(t, r, ActionStay, ReasonNothingUsable, "c", "c")
	parked := ob("a", 1, 1)
	parked.Parked = true
	r = decide(cfg, State{}, "b", true, parked, ob("b", 99, 99), ob("c", 1, 1))
	want(t, r, ActionSwitch, ReasonOverLimit, "b", "c")
}

func TestProtectedProfilesAreNeverATarget(t *testing.T) {
	cfg := claudeCfg()
	cfg.Profiles = cfg.Profiles[:2] // a, b listed
	cfg.Protected = []string{"c"}
	// c is perfectly usable but protected and unlisted: ignored. a and b are exhausted.
	r := decide(cfg, State{}, "b", true, ob("a", 99, 99), ob("b", 99, 99), ob("c", 1, 1))
	want(t, r, ActionStay, ReasonNothingUsable, "b", "b")
}

func TestNearRefreshHoldsOnlyAnUnmigratedTool(t *testing.T) {
	near := ob("a", 1, 1)
	soon := t0.Add(10 * time.Minute)
	near.TokenExpiresAt = &soon
	args := []Obs{near, ob("b", 1, 1), ob("c", 1, 1)}
	want(t, decide(claudeCfg(), State{}, "b", false, args...), ActionHold, ReasonNearRefresh, "b", "a")
	want(t, decide(claudeCfg(), State{}, "b", true, args...), ActionSwitch, ReasonHigherUsable, "b", "a")
	// An unknown expiry counts as near on the link model.
	unknown := ob("a", 1, 1)
	unknown.TokenExpiresAt = nil
	want(t, decide(claudeCfg(), State{}, "b", false, unknown, ob("b", 1, 1), ob("c", 1, 1)), ActionHold, ReasonNearRefresh, "b", "a")
	// The active side counts too.
	activeNear := ob("b", 1, 1)
	activeNear.TokenExpiresAt = &soon
	want(t, decide(claudeCfg(), State{}, "b", false, ob("a", 1, 1), activeNear, ob("c", 1, 1)), ActionHold, ReasonNearRefresh, "b", "a")
}

func TestDwellHoldsASecondSwitch(t *testing.T) {
	last := t0.Add(-4*time.Minute - 59*time.Second)
	args := []Obs{ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1)}
	want(t, decide(claudeCfg(), State{LastSwitchAt: &last}, "b", true, args...), ActionHold, ReasonSwitchedRecent, "b", "a")
	last = t0.Add(-5 * time.Minute)
	want(t, decide(claudeCfg(), State{LastSwitchAt: &last}, "b", true, args...), ActionSwitch, ReasonHigherUsable, "b", "a")
	zero := claudeCfg()
	zero.MinDwellMinutes = 0
	last = t0.Add(-time.Second)
	want(t, decide(zero, State{LastSwitchAt: &last}, "b", true, args...), ActionSwitch, ReasonHigherUsable, "b", "a")
}

func TestDryRunIsRecordedOnTheDecision(t *testing.T) {
	cfg := claudeCfg()
	cfg.DryRun = true
	r := decide(cfg, State{}, "b", true, ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
	if !r.Decision.DryRun {
		t.Fatal("the decision must say it was a dry run")
	}
}

// ── the reset latch ─────────────────────────────────────────────────────────

func resetCfg() Config {
	c := claudeCfg()
	// Before the latch: a, b, c. After the watched profile a has reset: b, a, c.
	c.AfterReset = &AfterReset{Profile: "a", DropPoints: 20, Profiles: []Limit{
		{Name: "b", SessionMax: 95, WeeklyMax: 95}, {Name: "a", SessionMax: 95, WeeklyMax: 95}, {Name: "c", SessionMax: 95, WeeklyMax: 95},
	}}
	return c
}

func TestTheResetLatchSwitchesTheOrderAndPersistsInTheState(t *testing.T) {
	cfg := resetCfg()
	args := func(aWeekly float64) []Obs { return []Obs{ob("a", 1, aWeekly), ob("b", 1, 10), ob("c", 1, 10)} }
	// a climbs to 84: the maximum follows, the order is the first one (a first).
	r := decide(cfg, State{}, "b", true, args(84)...)
	want(t, r, ActionSwitch, ReasonHigherUsable, "b", "a")
	if r.State.WeeklyMaxSeen == nil || *r.State.WeeklyMaxSeen != 84 || r.State.ResetLatchedAt != nil {
		t.Fatalf("state after the climb: %+v", r.State)
	}
	// 65 is 19 points below: no reset.
	r = decide(cfg, r.State, "b", true, args(65)...)
	if r.State.ResetLatchedAt != nil {
		t.Fatal("19 points is not a reset")
	}
	// 64 is exactly 20 below: reset, the other order applies at once (b before a).
	r = decide(cfg, r.State, "b", true, args(64)...)
	if r.State.ResetLatchedAt == nil {
		t.Fatal("20 points is a reset")
	}
	want(t, r, ActionStay, ReasonActiveIsBest, "b", "b")
	// Latched: it stays latched and the maximum stops moving, even if a climbs again.
	max := *r.State.WeeklyMaxSeen
	r = decide(cfg, r.State, "b", true, args(94)...)
	if r.State.ResetLatchedAt == nil || *r.State.WeeklyMaxSeen != max {
		t.Fatalf("a latch must hold: %+v", r.State)
	}
}

func TestANonOkSnapshotNeverMovesTheMaximumOrLooksLikeAReset(t *testing.T) {
	cfg := resetCfg()
	hi := decide(cfg, State{}, "b", true, ob("a", 1, 80), ob("b", 1, 10), ob("c", 1, 10)).State
	zero := ob("a", 1, 0)
	zero.Status = "refreshing"
	r := decide(cfg, hi, "b", true, zero, ob("b", 1, 10), ob("c", 1, 10))
	if r.State.ResetLatchedAt != nil || *r.State.WeeklyMaxSeen != 80 {
		t.Fatalf("a refreshing zero must not look like a reset: %+v", r.State)
	}
	stale := ob("a", 1, 0)
	stale.Stale = true
	r = decide(cfg, hi, "b", true, stale, ob("b", 1, 10), ob("c", 1, 10))
	if r.State.ResetLatchedAt != nil {
		t.Fatal("a stale zero must not look like a reset")
	}
}

func TestResetTrackingOnlyRunsWhileEnabled(t *testing.T) {
	cfg := resetCfg()
	cfg.Enabled = false
	r := decide(cfg, State{}, "b", true, ob("a", 1, 80), ob("b", 1, 10), ob("c", 1, 10))
	if r.State.WeeklyMaxSeen != nil {
		t.Fatalf("a disabled tool tracks nothing: %+v", r.State)
	}
}

func TestDecideDoesNotMutateItsInput(t *testing.T) {
	cfg := resetCfg()
	in := Input{Tool: ToolClaude, Config: cfg, Profiles: []Obs{ob("a", 1, 80), ob("b", 1, 10), ob("c", 1, 10)}, Active: "b", Now: t0, Migrated: true}
	before := in.State
	Decide(in)
	if in.State.WeeklyMaxSeen != before.WeeklyMaxSeen || in.State.EvaluatedAt != nil {
		t.Fatal("Decide changed the state it was given")
	}
}

// ── validation ──────────────────────────────────────────────────────────────

func TestValidate(t *testing.T) {
	known := []string{"a", "b", "c"}
	good := claudeCfg()
	if err := good.Validate(known); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(*Config)) Config {
		c := claudeCfg()
		f(&c)
		return c
	}
	for name, c := range map[string]Config{
		"unknown profile":      mut(func(c *Config) { c.Profiles[0].Name = "zzz" }),
		"duplicate":            mut(func(c *Config) { c.Profiles[1].Name = "a" }),
		"limit zero":           mut(func(c *Config) { c.Profiles[0].SessionMax = 0 }),
		"limit over 100":       mut(func(c *Config) { c.Profiles[0].WeeklyMax = 101 }),
		"listed and protected": mut(func(c *Config) { c.Protected = []string{"a"} }),
		"unknown protected":    mut(func(c *Config) { c.Protected = []string{"zzz"} }),
		"one profile enabled":  mut(func(c *Config) { c.Profiles = c.Profiles[:1] }),
		"warmup too long":      mut(func(c *Config) { c.WarmupSeconds = 601 }),
		"dwell negative":       mut(func(c *Config) { c.MinDwellMinutes = -1 }),
		"reset unknown watch":  mut(func(c *Config) { c.AfterReset = &AfterReset{Profile: "zzz", DropPoints: 20} }),
		"reset zero drop":      mut(func(c *Config) { c.AfterReset = &AfterReset{Profile: "a", DropPoints: 0} }),
		"reset one profile": mut(func(c *Config) {
			c.AfterReset = &AfterReset{Profile: "a", DropPoints: 20, Profiles: []Limit{{Name: "a", SessionMax: 95, WeeklyMax: 95}}}
		}),
		"reset no profiles": mut(func(c *Config) { c.AfterReset = &AfterReset{Profile: "a", DropPoints: 20} }),
	} {
		if err := c.Validate(known); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A disabled config with one profile is a draft, not an error.
	draft := mut(func(c *Config) { c.Enabled = false; c.Profiles = c.Profiles[:1] })
	if err := draft.Validate(known); err != nil {
		t.Fatalf("a disabled draft must validate: %v", err)
	}
}
