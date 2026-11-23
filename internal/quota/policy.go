package quota

import (
	"time"
)

// Actions of a Decision.
const (
	ActionIdle   = "idle"   // automation is off or has too little to work with
	ActionStay   = "stay"   // the active profile is the right one (or nothing is usable)
	ActionHold   = "hold"   // a safety rule says do not switch now
	ActionSwitch = "switch" // switch From to To
	ActionError  = "error"  // the switch was attempted and failed
)

// Fixed reasons. A reason is one of these sentences, never error text, so it is safe
// to store, show and put in an event.
const (
	ReasonOff             = "automatic switching is off"
	ReasonTooFew          = "fewer than two listed profiles exist"
	ReasonTripped         = "automatic switching tripped; re-enable it in Settings"
	ReasonNoActive        = "no active profile"
	ReasonProtected       = "a protected profile is active"
	ReasonActiveNotOK     = "active snapshot not ok"
	ReasonWarmingUp       = "warming up"
	ReasonNothingUsable   = "no listed profile is usable; staying put"
	ReasonActiveIsBest    = "the active profile is the highest-priority usable one"
	ReasonNearRefresh     = "near token refresh"
	ReasonSwitchedRecent  = "switched recently"
	ReasonOverLimit       = "the active profile is over its limit or unusable"
	ReasonActiveNotListed = "the active profile is not in the order"
	ReasonHigherUsable    = "a higher-priority profile is usable again"

	// Reasons the runner gives to what happens around the switch itself.
	ReasonActiveChanged = "the active profile changed; deciding again"
	ReasonRefreshFailed = "incoming refresh failed"
	ReasonAccountBusy   = "account busy"
	ReasonLoginPending  = "login pending"
	ReasonSwitchFailed  = "switch failed"
	ReasonBreaker       = "too many automatic switch attempts this hour"
	ReasonRearmed       = "re-armed by the owner"
	ReasonReenabled     = "re-enabled by the owner"
	ReasonResetSeen     = "the watched weekly window reset"
	ReasonBackingOff    = "retrying after a failure"
	ReasonSwitcherOff   = "the account switcher is disabled"
	ReasonNoSnapshot    = "no usage snapshot yet"
	nearRefreshWindow   = 15 * time.Minute
)

// Obs is what the policy knows about one profile at one instant.
type Obs struct {
	Name   string
	Status string // the usage snapshot status; "ok" is the only healthy one
	Stale  bool
	// Session and Weekly are percent used; nil when the provider reported no such
	// window.
	Session, Weekly *float64
	// OKFor is how long every observation of this profile has been ok (0 when the
	// latest one was not).
	OKFor          time.Duration
	HasCredentials bool
	// Parked: the token refresher gave up on the account (reauth_required or
	// needs_relogin_soon); it is never a target.
	Parked bool
	// TokenExpiresAt is the stored token's expiry, nil when unknown.
	TokenExpiresAt *time.Time
	// Limited: a harness of the tool on the host machine met the provider's usage limit on this login (a provider limit is in force for one of
	// its agents), whatever the usage snapshot says. Only the active profile can carry it. A limited profile is exhausted: never usable.
	Limited bool
	// ObservedAt is when the usage reader last read this profile (the snapshot's own
	// timestamp), nil when it has none. The runner measures the warm-up in these times
	// and marks a reading that is too old as stale.
	ObservedAt *time.Time
}

// OK reports whether the snapshot is a healthy, fresh reading.
func (o Obs) OK() bool { return o.Status == "ok" && !o.Stale }

// State is what persists between evaluations.
type State struct {
	ResetLatchedAt *time.Time `json:"reset_latched_at"`
	WeeklyMaxSeen  *float64   `json:"weekly_max_seen"`
	LastSwitchAt   *time.Time `json:"last_switch_at"`
	Tripped        bool       `json:"tripped"`
	TrippedAt      *time.Time `json:"tripped_at"`
	// Hold is the current hold's reason, "" when there is none.
	Hold        string     `json:"hold"`
	EvaluatedAt *time.Time `json:"evaluated_at"`
}

// ProfileView is one profile's line in a Decision's record of what it saw.
type ProfileView struct {
	Status       string   `json:"status"`
	Session      *float64 `json:"session"`
	Weekly       *float64 `json:"weekly"`
	OKForSeconds float64  `json:"ok_for_seconds"`
	Limited      bool     `json:"limited,omitempty"`
}

// Decision is the structured outcome of one evaluation.
type Decision struct {
	At       time.Time              `json:"at"`
	Tool     string                 `json:"tool"`
	Action   string                 `json:"action"`
	From     *string                `json:"from"`
	To       *string                `json:"to"`
	Reason   string                 `json:"reason"`
	Profiles map[string]ProfileView `json:"profiles"`
}

// Same reports whether two decisions say the same thing, ignoring the time and the
// numbers: the runner records a change, not every evaluation.
func (d Decision) Same(o Decision) bool {
	return d.Tool == o.Tool && d.Action == o.Action && d.Reason == o.Reason &&
		eq(d.From, o.From) && eq(d.To, o.To)
}

func eq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Input is everything Decide needs. It reads no clock, file or network.
type Input struct {
	Tool     string
	Config   Config
	State    State
	Profiles []Obs
	// Active is the active profile of the tool, "" when none.
	Active string
	Now    time.Time
	// Migrated says the tool runs the token-copy model, where a switch refreshes the
	// incoming profile under the account lock and the near-refresh hold is not needed.
	Migrated bool
}

// Result is the decision and the state after it (the reset latch and the highest
// weekly value seen move here; a switch's timestamp and the breaker are the
// runner's).
type Result struct {
	Decision Decision
	State    State
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// usable: ok and settled, under both limits, with credentials and not parked.
func usable(o Obs, l Limit, warmup time.Duration) bool {
	if !o.OK() || o.OKFor < warmup || !o.HasCredentials || o.Parked || o.Limited {
		return false
	}
	if o.Session != nil && !(*o.Session < l.SessionMax) {
		return false
	}
	return o.Weekly != nil && *o.Weekly < l.WeeklyMax
}

func nearRefresh(o Obs, now time.Time) bool {
	return o.TokenExpiresAt == nil || o.TokenExpiresAt.Sub(now) < nearRefreshWindow
}

// Decide is the whole policy, in the order of docs/design/quota-switcher.md:
// off, too few, tripped, no active, protected active, unclear active, warm-up,
// choose, stay, near-refresh (link model only), dwell, switch.
func Decide(in Input) Result {
	cfg := in.Config
	now := in.Now
	state := in.State
	obs := map[string]Obs{}
	for _, o := range in.Profiles {
		obs[o.Name] = o
	}
	d := Decision{At: now, Tool: in.Tool, Profiles: map[string]ProfileView{}}
	for _, o := range in.Profiles {
		d.Profiles[o.Name] = ProfileView{Status: viewStatus(o), Session: o.Session, Weekly: o.Weekly, OKForSeconds: o.OKFor.Seconds(), Limited: o.Limited}
	}
	finish := func(action, reason string) Result {
		d.Action, d.Reason = action, reason
		if action == ActionHold {
			state.Hold = reason
		} else {
			state.Hold = ""
		}
		at := now
		state.EvaluatedAt = &at
		return Result{Decision: d, State: state}
	}

	if !cfg.Enabled {
		return finish(ActionIdle, ReasonOff)
	}

	// The reset latch: the watched profile's weekly window dropping DropPoints
	// below the highest value seen means it reset. Only an ok snapshot moves the
	// maximum (a stale zero must not look like a reset), and nothing moves once
	// latched; the owner re-arms by hand.
	if a := cfg.AfterReset; a != nil && state.ResetLatchedAt == nil {
		if o, ok := obs[a.Profile]; ok && o.OK() && o.Weekly != nil {
			w := *o.Weekly
			if state.WeeklyMaxSeen == nil || w > *state.WeeklyMaxSeen {
				m := w
				state.WeeklyMaxSeen = &m
			}
			if *state.WeeklyMaxSeen-w >= a.DropPoints {
				at := now
				state.ResetLatchedAt = &at
			}
		}
	}
	order := cfg.Profiles
	if cfg.AfterReset != nil && state.ResetLatchedAt != nil {
		order = cfg.AfterReset.Profiles
	}
	protected := map[string]bool{}
	for _, name := range cfg.Protected {
		protected[name] = true
	}
	var candidates []Limit
	for _, l := range order {
		if _, exists := obs[l.Name]; exists && !protected[l.Name] {
			candidates = append(candidates, l)
		}
	}
	if len(candidates) < 2 {
		return finish(ActionIdle, ReasonTooFew)
	}
	if in.Active != "" {
		d.From = strp(in.Active)
	}
	if state.Tripped {
		return finish(ActionHold, ReasonTripped)
	}
	if in.Active == "" {
		return finish(ActionHold, ReasonNoActive)
	}
	if protected[in.Active] {
		return finish(ActionHold, ReasonProtected)
	}
	if active, ok := obs[in.Active]; !ok || !active.OK() {
		return finish(ActionHold, ReasonActiveNotOK)
	}
	warmup := time.Duration(cfg.WarmupSeconds) * time.Second
	for _, o := range in.Profiles {
		if o.OK() && o.OKFor < warmup {
			return finish(ActionHold, ReasonWarmingUp)
		}
	}

	want, found := "", false
	var activeLimit *Limit
	for i, l := range candidates {
		if l.Name == in.Active {
			activeLimit = &candidates[i]
		}
		if !found && usable(obs[l.Name], l, warmup) {
			want, found = l.Name, true
		}
	}
	if !found {
		d.To = strp(in.Active)
		return finish(ActionStay, ReasonNothingUsable)
	}
	if want == in.Active {
		d.To = strp(in.Active)
		return finish(ActionStay, ReasonActiveIsBest)
	}
	d.To = strp(want)
	// A migrated switch refreshes the incoming account itself before the swap,
	// with maintenance locks on both accounts for the full operation. The
	// near-expiry hold is for link-model credentials that the server cannot
	// refresh safely on demand.
	if !in.Migrated && (nearRefresh(obs[in.Active], now) || nearRefresh(obs[want], now)) {
		return finish(ActionHold, ReasonNearRefresh)
	}
	if state.LastSwitchAt != nil && cfg.MinDwellMinutes > 0 && now.Sub(*state.LastSwitchAt) < time.Duration(cfg.MinDwellMinutes)*time.Minute {
		return finish(ActionHold, ReasonSwitchedRecent)
	}
	reason := ReasonHigherUsable
	switch {
	case activeLimit == nil:
		// The active profile is not in the order (or not in the after-reset order): the owner never gave it a
		// place, so the switcher moves off it, and says why in those words.
		reason = ReasonActiveNotListed
	case !usable(obs[in.Active], *activeLimit, warmup):
		reason = ReasonOverLimit
	}
	return finish(ActionSwitch, reason)
}

func viewStatus(o Obs) string {
	if o.Stale && o.Status == "ok" {
		return "stale"
	}
	return o.Status
}
