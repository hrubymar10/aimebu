// Package quota is the automatic account switcher: for each of Claude and Codex
// an owner-defined ordered list of profiles, switched between by the first one
// whose session and weekly usage are under its limits. Decide (policy.go) is a pure
// function of its inputs; the Runner (runner.go) feeds it snapshots and acts on its
// answer through the switcher.
package quota

import (
	"errors"
	"fmt"
	"strings"
)

// Tool names the switcher's two tools, as the switcher spells them.
const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// Tools lists the tools the quota switcher manages.
func Tools() []string { return []string{ToolClaude, ToolCodex} }

// Limit is one entry of an ordered list: a profile and the usage (percent of the
// window already used) at or above which it is no longer usable. Both comparisons
// are strict: a profile at exactly its limit is exhausted.
type Limit struct {
	Name       string  `json:"name"`
	SessionMax float64 `json:"session_max"`
	WeeklyMax  float64 `json:"weekly_max"`
}

// AfterReset is the order that applies once the watched profile's weekly window has
// been seen to reset (its weekly usage dropped DropPoints below the highest value
// seen). The latch is persisted and re-armed by hand only.
type AfterReset struct {
	Profile    string  `json:"profile"`
	DropPoints float64 `json:"drop_points"`
	Profiles   []Limit `json:"profiles"`
}

// Config is one tool's settings.
type Config struct {
	Enabled bool `json:"enabled"`
	// Profiles is the order: the first usable entry wins.
	Profiles []Limit `json:"profiles"`
	// Protected profiles are never switched away from or to; while one is active the
	// tool is on hold.
	Protected  []string    `json:"protected"`
	AfterReset *AfterReset `json:"after_reset"`
	// WarmupSeconds is how long a profile must have been continuously ok before it
	// counts, or before a switch is made while it is settling.
	WarmupSeconds   int `json:"warmup_seconds"`
	MinDwellMinutes int `json:"min_dwell_minutes"`
}

// Defaults of the numbers the owner can tune.
const (
	DefaultWarmupSeconds   = 60
	DefaultMinDwellMinutes = 5
	// DefaultDropPoints is how far a weekly percentage must fall below its highest
	// value to count as a reset.
	DefaultDropPoints = 20
)

// Default is the pre-filled configuration of tool for the profiles that exist, in
// the order given: today's thresholds by position (Claude 95/95 for every entry;
// Codex 93/98 for the first entry and 98/98 for the rest). It is disabled, and it names nothing but the profiles it was given.
func Default(tool string, names []string) Config {
	c := Config{WarmupSeconds: DefaultWarmupSeconds, MinDwellMinutes: DefaultMinDwellMinutes, Profiles: []Limit{}, Protected: []string{}}
	for i, name := range names {
		l := Limit{Name: name, SessionMax: 95, WeeklyMax: 95}
		if tool == ToolCodex {
			l.SessionMax, l.WeeklyMax = 98, 98
			if i == 0 {
				l.SessionMax, l.WeeklyMax = 93, 98
			}
		}
		c.Profiles = append(c.Profiles, l)
	}
	return c
}

// Normalize makes the nil slices of a decoded Config empty, so it always encodes
// as arrays.
func (c *Config) Normalize() {
	if c.Profiles == nil {
		c.Profiles = []Limit{}
	}
	if c.Protected == nil {
		c.Protected = []string{}
	}
	if c.AfterReset != nil && c.AfterReset.Profiles == nil {
		c.AfterReset.Profiles = []Limit{}
	}
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid quota switcher config")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks c for tool. known is the set of profile names that exist in the
// store; every name in the lists must be one of them (a name that disappears later
// is skipped by the policy, not rejected here).
func (c Config) Validate(known []string) error {
	exists := map[string]bool{}
	for _, name := range known {
		exists[name] = true
	}
	checkList := func(label string, list []Limit) error {
		seen := map[string]bool{}
		for _, l := range list {
			if strings.TrimSpace(l.Name) == "" {
				return invalid("%s has an entry without a profile name", label)
			}
			if !exists[l.Name] {
				return invalid("%s names %q, which is not a profile", label, l.Name)
			}
			if seen[l.Name] {
				return invalid("%s lists %q twice", label, l.Name)
			}
			seen[l.Name] = true
			if !(l.SessionMax >= 1 && l.SessionMax <= 100) || !(l.WeeklyMax >= 1 && l.WeeklyMax <= 100) {
				return invalid("%s: the limits of %q must be between 1 and 100", label, l.Name)
			}
		}
		return nil
	}
	if err := checkList("profiles", c.Profiles); err != nil {
		return err
	}
	protected := map[string]bool{}
	for _, name := range c.Protected {
		if !exists[name] {
			return invalid("protected names %q, which is not a profile", name)
		}
		if protected[name] {
			return invalid("protected lists %q twice", name)
		}
		protected[name] = true
	}
	for _, l := range c.Profiles {
		if protected[l.Name] {
			return invalid("%q cannot be both listed and protected", l.Name)
		}
	}
	if c.AfterReset != nil {
		a := c.AfterReset
		if !exists[a.Profile] {
			return invalid("after_reset watches %q, which is not a profile", a.Profile)
		}
		if !(a.DropPoints >= 1 && a.DropPoints <= 100) {
			return invalid("after_reset drop_points must be between 1 and 100")
		}
		if err := checkList("after_reset profiles", a.Profiles); err != nil {
			return err
		}
		for _, l := range a.Profiles {
			if protected[l.Name] {
				return invalid("%q cannot be both listed and protected", l.Name)
			}
		}
	}
	if c.WarmupSeconds < 0 || c.WarmupSeconds > 600 {
		return invalid("warmup_seconds must be between 0 and 600")
	}
	if c.MinDwellMinutes < 0 || c.MinDwellMinutes > 120 {
		return invalid("min_dwell_minutes must be between 0 and 120")
	}
	if c.Enabled && len(c.Profiles) < 2 {
		return invalid("at least two profiles are needed to enable automatic switching")
	}
	// Once the reset latches, the after-reset order replaces the main one, and an order of fewer than
	// two profiles cannot switch: the tool would sit idle for the rest of the week.
	if c.Enabled && c.AfterReset != nil && len(c.AfterReset.Profiles) < 2 {
		return invalid("after_reset needs at least two profiles to enable automatic switching")
	}
	return nil
}
