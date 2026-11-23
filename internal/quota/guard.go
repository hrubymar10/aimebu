package quota

import (
	"fmt"
	"time"
)

// The limit guard acts before a window is used up. For each tool the owner sets a soft
// threshold (the active profile no longer starts new turns; the running one finishes) and a
// hard threshold (the profile is at its limit: the owner is told, and new turns stay held). The
// quota switcher treats the soft threshold as the one to leave a profile at (ClampToSoft), so with
// another profile usable it moves at once and the hold is short. Both thresholds are percent used
// of the fuller of the session and the weekly window, and both compare with >=.

// Guard defaults. Soft must stay below hard. The soft default is the tool's own switcher limit (the
// Claude and Codex defaults of Default), so turning the guard on changes nothing until the owner
// lowers a threshold; hard is where the owner is told the profile is at its limit.
const DefaultGuardHard = 99

// DefaultGuardSoft is the soft threshold a tool starts with.
func DefaultGuardSoft(tool string) float64 {
	if tool == ToolCodex {
		return 98
	}
	return 95
}

// GuardLimit is one profile's own soft and hard thresholds, replacing the tool's.
type GuardLimit struct {
	Soft float64 `json:"soft"`
	Hard float64 `json:"hard"`
}

// GuardConfig is one tool's guard settings.
type GuardConfig struct {
	Enabled bool    `json:"enabled"`
	Soft    float64 `json:"soft"`
	Hard    float64 `json:"hard"`
	// Profiles overrides Soft and Hard per profile name.
	Profiles map[string]GuardLimit `json:"profiles"`
}

// DefaultGuard is on, with tool's default thresholds and no overrides.
func DefaultGuard(tool string) GuardConfig {
	return GuardConfig{Enabled: true, Soft: DefaultGuardSoft(tool), Hard: DefaultGuardHard, Profiles: map[string]GuardLimit{}}
}

// Normalize makes a decoded config encode its overrides as an object.
func (g *GuardConfig) Normalize() {
	if g.Profiles == nil {
		g.Profiles = map[string]GuardLimit{}
	}
}

// Limits returns the thresholds that apply to profile.
func (g GuardConfig) Limits(profile string) GuardLimit {
	if l, ok := g.Profiles[profile]; ok {
		return l
	}
	return GuardLimit{Soft: g.Soft, Hard: g.Hard}
}

// Validate checks the thresholds; known is the set of profile names that exist.
func (g GuardConfig) Validate(known []string) error {
	check := func(label string, l GuardLimit) error {
		if !(l.Soft >= 1 && l.Soft <= 100) || !(l.Hard >= 1 && l.Hard <= 100) {
			return invalid("%s: thresholds must be between 1 and 100", label)
		}
		if !(l.Soft < l.Hard) {
			return invalid("%s: the soft threshold must be below the hard one", label)
		}
		return nil
	}
	if err := check("limit guard", GuardLimit{Soft: g.Soft, Hard: g.Hard}); err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, name := range known {
		exists[name] = true
	}
	for name, l := range g.Profiles {
		if !exists[name] {
			return invalid("limit guard overrides %q, which is not a profile", name)
		}
		if err := check(fmt.Sprintf("limit guard for %q", name), l); err != nil {
			return err
		}
	}
	return nil
}

// Guard states, from the quietest to the most severe.
const (
	GuardOff     = "off"     // the guard is disabled for the tool
	GuardUnknown = "unknown" // no clear reading of the active profile: nothing is held on unclear data
	GuardOK      = "ok"
	GuardSoft    = "soft" // new turns are held
	GuardHard    = "hard" // the profile counts as exhausted
)

// GuardResult is the guard's verdict on the active profile.
type GuardResult struct {
	State   string   `json:"state"`
	Profile string   `json:"profile"`
	Percent *float64 `json:"percent"` // the fuller window, nil when unknown
	Window  string   `json:"window"`  // "session" or "weekly"
	Soft    float64  `json:"soft"`
	Hard    float64  `json:"hard"`
}

// EvaluateGuard judges the active profile. Only a healthy, fresh reading is acted on.
func EvaluateGuard(g GuardConfig, active Obs) GuardResult {
	l := g.Limits(active.Name)
	res := GuardResult{State: GuardOff, Profile: active.Name, Soft: l.Soft, Hard: l.Hard}
	if !g.Enabled {
		return res
	}
	res.State = GuardUnknown
	if active.Name == "" || !active.OK() {
		return res
	}
	for _, w := range []struct {
		name string
		v    *float64
	}{{"session", active.Session}, {"weekly", active.Weekly}} {
		if w.v != nil && (res.Percent == nil || *w.v > *res.Percent) {
			v := *w.v
			res.Percent, res.Window = &v, w.name
		}
	}
	switch {
	case res.Percent == nil:
	case *res.Percent >= l.Hard:
		res.State = GuardHard
	case *res.Percent >= l.Soft:
		res.State = GuardSoft
	default:
		res.State = GuardOK
	}
	return res
}

// ClampToSoft returns cfg with every profile's limits lowered to its soft threshold, so the quota
// switcher treats a profile at the soft threshold as used up and moves off it while another profile
// is usable, instead of leaving new turns held until the window resets. A switcher limit that is
// already lower stays. With the guard off, cfg is returned unchanged.
func ClampToSoft(cfg Config, g GuardConfig) Config {
	if !g.Enabled {
		return cfg
	}
	clamp := func(list []Limit) []Limit {
		out := make([]Limit, len(list))
		for i, l := range list {
			soft := g.Limits(l.Name).Soft
			if soft < l.SessionMax {
				l.SessionMax = soft
			}
			if soft < l.WeeklyMax {
				l.WeeklyMax = soft
			}
			out[i] = l
		}
		return out
	}
	cfg.Profiles = clamp(cfg.Profiles)
	if cfg.AfterReset != nil {
		a := *cfg.AfterReset
		a.Profiles = clamp(a.Profiles)
		cfg.AfterReset = &a
	}
	return cfg
}

// AgeObs marks a reading older than maxAge stale (zero disables the check). The runner and the guard
// both use it, so a stalled usage poller makes both hold back instead of acting on old percentages.
func AgeObs(profiles []Obs, now time.Time, maxAge time.Duration) {
	for i := range profiles {
		if profiles[i].ObservedAt != nil && maxAge > 0 && now.Sub(*profiles[i].ObservedAt) > maxAge {
			profiles[i].Stale = true
		}
	}
}

// HasTarget reports whether the quota switcher could move tool off its active profile: it really
// switches (enabled, not tripped), the active profile is not protected, and some
// other listed profile is usable under the guard's soft threshold. Warm-up and dwell only delay a
// switch, so they do not count against it.
func HasTarget(cfg Config, state State, g GuardConfig, obs Observation) bool {
	if !cfg.Enabled || state.Tripped || obs.Active == "" {
		return false
	}
	for _, name := range cfg.Protected {
		if name == obs.Active {
			return false
		}
	}
	clamped := ClampToSoft(cfg, g)
	order := clamped.Profiles
	if clamped.AfterReset != nil && state.ResetLatchedAt != nil {
		order = clamped.AfterReset.Profiles
	}
	byName := map[string]Obs{}
	for _, o := range obs.Profiles {
		byName[o.Name] = o
	}
	for _, l := range order {
		if l.Name == obs.Active {
			continue
		}
		if o, ok := byName[l.Name]; ok && usable(o, l, 0) {
			return true
		}
	}
	return false
}
