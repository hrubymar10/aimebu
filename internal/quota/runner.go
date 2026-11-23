package quota

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// Persistence is what the runner needs from the store (*store.Store satisfies it).
type Persistence interface {
	QuotaConfig(ctx context.Context, tool string) (string, bool, error)
	SaveQuotaConfig(ctx context.Context, tool, document string) error
	QuotaState(ctx context.Context, tool string) (string, bool, error)
	SaveQuotaState(ctx context.Context, tool, document string) error
	AppendQuotaDecision(ctx context.Context, tool, at, document string) error
	AppendQuotaHoldDecision(ctx context.Context, tool, at, document string, payload map[string]string) error
	QuotaDecisions(ctx context.Context, tool string, limit int) ([]string, error)
}

// Observation is the runner's view of one tool at one instant, built by the server
// from the usage snapshot, the switcher and the token refresher.
type Observation struct {
	// Profiles are the readings of every profile. The runner fills in OKFor.
	Profiles []Obs
	Active   string
	// Migrated: the tool runs the token-copy model.
	Migrated bool
	// SwitcherEnabled: the account switcher is on at all.
	SwitcherEnabled bool
	// Ready is false until the usage reader has produced a snapshot for the tool.
	Ready bool
	// MaxAge is how old a profile's reading may be before the runner treats it as
	// stale; zero disables the check.
	MaxAge time.Duration
	// Solo is the implicit single profile of a tool with no stored profile ("local"). The runner
	// cannot switch between profiles it does not have, so it is not in Profiles; the limit guard
	// still protects it.
	Solo *Obs
}

// Source feeds the runner.
type Source interface {
	Observe(ctx context.Context, tool string) (Observation, error)
	// Profiles lists the profile names of tool that exist in the store.
	Profiles(ctx context.Context, tool string) ([]string, error)
}

// Actuator performs a switch. switched reports whether the active profile really
// changed: false with no hold and no error means the target was already active, which
// is not a switch and is not counted as one. hold != "" is a retryable hold (one of the
// fixed reasons); err is a failure whose text stays in the log. from is the profile the
// decision was made from: the actuator must refuse (a hold) when another profile is
// active by the time it holds the account lock, so a switch the owner made in between
// is never overridden.
type Actuator interface {
	Switch(ctx context.Context, tool, from, to string) (switched bool, hold string, err error)
}

// Events receives the operator-facing events. Payloads carry tool names and fixed
// reasons only.
type Events interface {
	Event(kind string, payload map[string]string)
}

// Tunables of the runner (not of the policy).
const (
	// BreakerSwitches is how many automatic switch attempts of a tool (switches and failed attempts) one hour may hold;
	// the next one trips the breaker instead.
	BreakerSwitches = 4
	breakerWindow   = time.Hour
	tickInterval    = 15 * time.Second
	maxBackoff      = 5 * time.Minute
)

// persisted is the state document: the API's State plus what only the runner needs.
type persisted struct {
	State
	RecentSwitches []time.Time `json:"recent_switches,omitempty"`
}

// Runner evaluates the policy and acts on it. One goroutine (Run) does the work;
// the API methods only read and write the stored documents under the same mutex.
type Runner struct {
	store Persistence
	src   Source
	act   Actuator
	ev    Events
	now   func() time.Time

	trigger chan struct{}
	// guard, when set, supplies the limit guard's settings: the switcher then treats a profile at the guard's
	// hard threshold as exhausted.
	guard func(ctx context.Context, tool string) GuardConfig

	// evalMu serialises everything that changes a tool's stored state: an evaluation
	// pass, SaveConfig, Rearm and Reenable. mu guards the in-memory maps and the reads
	// of the API; an evaluation holds it except while the switch itself runs, so the
	// API stays responsive during a slow switch while a write waits for it to end.
	evalMu  sync.Mutex
	mu      sync.Mutex
	okSince map[string]okTrack  // "tool/profile" -> the run of ok readings
	good    map[string]goodRead // "tool/profile" -> the last ok reading, to bridge a transient read failure
	quiet   map[string]quietRun // tool -> the run of transient holds, announced only once it lasts
	last    map[string]Decision // tool -> the decision last recorded
	mem     map[string]memState // tool -> what is shown but not persisted
	backoff map[string]backoff
}

type memState struct {
	Hold        string
	EvaluatedAt *time.Time
}

type backoff struct {
	failures int
	next     time.Time
	// lastErr is the text of the failure last announced in this run of failures: a repeat of the same
	// failure is retried and logged, not announced again.
	lastErr string
}

// NewRunner builds a runner. ev may be nil.
func NewRunner(store Persistence, src Source, act Actuator, ev Events) *Runner {
	return &Runner{store: store, src: src, act: act, ev: ev, now: time.Now, trigger: make(chan struct{}, 1),
		okSince: map[string]okTrack{}, good: map[string]goodRead{}, quiet: map[string]quietRun{}, last: map[string]Decision{}, mem: map[string]memState{}, backoff: map[string]backoff{}}
}

// CanSwitch reports whether the runner could move tool off the active profile of obs right now (see
// HasTarget). The limit guard uses it to say whether a hold is about to be lifted by a switch.
func (r *Runner) CanSwitch(ctx context.Context, tool string, obs Observation, g GuardConfig) bool {
	cfg, _, err := r.loadConfig(ctx, tool)
	if err != nil {
		return false
	}
	state, err := r.loadState(ctx, tool)
	if err != nil {
		return false
	}
	return HasTarget(cfg, state.State, g, obs)
}

// DryRunTarget reports whether tool is in dry run and would have a target if it were not: the limit guard's notice then says the switch is due but
// dry run keeps it from happening.
func (r *Runner) DryRunTarget(ctx context.Context, tool string, obs Observation, g GuardConfig) bool {
	cfg, _, err := r.loadConfig(ctx, tool)
	if err != nil || !cfg.DryRun {
		return false
	}
	state, err := r.loadState(ctx, tool)
	if err != nil {
		return false
	}
	cfg.DryRun = false
	return HasTarget(cfg, state.State, g, obs)
}

// SetGuard makes the runner clamp every profile's limits to the guard's soft threshold.
func (r *Runner) SetGuard(f func(ctx context.Context, tool string) GuardConfig) { r.guard = f }

// SetClock replaces the clock (tests).
func (r *Runner) SetClock(now func() time.Time) { r.now = now }

// Trigger asks for an evaluation soon.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run evaluates on every trigger and every tick until ctx ends. It blocks.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	r.Evaluate(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
		case <-ticker.C:
		}
		r.Evaluate(ctx)
	}
}

func (r *Runner) loadConfig(ctx context.Context, tool string) (Config, bool, error) {
	doc, found, err := r.store.QuotaConfig(ctx, tool)
	if err != nil {
		return Config{}, false, err
	}
	if !found {
		names, err := r.src.Profiles(ctx, tool)
		if err != nil {
			return Config{}, false, err
		}
		return Default(tool, names), false, nil
	}
	var cfg Config
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		return Config{}, false, fmt.Errorf("stored %s config: %w", tool, err)
	}
	cfg.Normalize()
	return cfg, true, nil
}

func (r *Runner) loadState(ctx context.Context, tool string) (persisted, error) {
	doc, found, err := r.store.QuotaState(ctx, tool)
	if err != nil || !found {
		return persisted{}, err
	}
	var p persisted
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return persisted{}, fmt.Errorf("stored %s state: %w", tool, err)
	}
	return p, nil
}

func (r *Runner) saveState(ctx context.Context, tool string, p persisted) error {
	p.Hold, p.EvaluatedAt = "", nil // shown from memory, never persisted
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return r.store.SaveQuotaState(ctx, tool, string(raw))
}

func (r *Runner) record(ctx context.Context, d Decision) {
	previous, known := r.last[d.Tool]
	if !known {
		// The last hold survives a server restart in the decision journal. Read it
		// before appending so a restart does not announce the same hold again.
		if docs, err := r.store.QuotaDecisions(ctx, d.Tool, 1); err == nil && len(docs) > 0 {
			if json.Unmarshal([]byte(docs[0]), &previous) == nil {
				known = true
			}
		}
	}
	if d.Profiles == nil {
		d.Profiles = map[string]ProfileView{}
	}
	if d.Action == ActionSwitch && d.DryRun && (!known || !previous.Same(d)) {
		// A dry run records the switch it would make and makes none. Said once per change (a restart that finds the same decision in the journal
		// stays quiet), so an owner who left dry run on learns that a switch was due instead of finding agents stalled at the limit.
		payload := map[string]string{"tool": d.Tool, "reason": d.Reason}
		if d.From != nil {
			payload["from"] = *d.From
		}
		if d.To != nil {
			payload["to"] = *d.To
		}
		r.event("quota_switch_dry_run", payload)
	}
	if d.Action != ActionHold || !transientReason(d.Reason) {
		delete(r.quiet, d.Tool)
	}
	raw, err := json.Marshal(d)
	if err == nil {
		at := d.At.UTC().Format(time.RFC3339Nano)
		if d.Action == ActionHold && r.announceHold(d, previous, known) {
			payload := map[string]string{"tool": d.Tool, "reason": d.Reason}
			if d.From != nil {
				payload["from"] = *d.From
			}
			if d.To != nil {
				payload["to"] = *d.To
			}
			err = r.store.AppendQuotaHoldDecision(ctx, d.Tool, at, string(raw), payload)
		} else {
			err = r.store.AppendQuotaDecision(ctx, d.Tool, at, string(raw))
		}
	}
	if err != nil {
		log.Printf("quota: record %s decision: %v", d.Tool, err)
		report("recording a switch decision", false)
		return
	}
	r.last[d.Tool] = d
}

// TransientHoldAnnounceAfter is how long a hold for missing, failed or settling data must last before it is
// announced as an event (and so reaches the bell).
const TransientHoldAnnounceAfter = 10 * time.Minute

// transientReason says whether a hold reason is only the data catching up: no snapshot yet, a reading that is
// not ok, a profile still warming up. These come and go with every hiccup of the usage poller.
func transientReason(reason string) bool {
	return reason == ReasonActiveNotOK || reason == ReasonWarmingUp || reason == ReasonNoSnapshot
}

// quietRun is one run of consecutive transient holds of a tool: since is its first decision's time.
type quietRun struct {
	since     time.Time
	announced bool
}

// longTransient says whether an unchanged transient hold has just crossed the announcement threshold, so it is
// recorded again once (which announces it) although the decision itself did not change.
func (r *Runner) longTransient(d Decision) bool {
	run, ok := r.quiet[d.Tool]
	return ok && !run.announced && d.Action == ActionHold && transientReason(d.Reason) && d.At.Sub(run.since) >= TransientHoldAnnounceAfter
}

// announceHold says whether hold decision d is an event. A real hold (tripped, protected, no active...) is one
// whenever its reason changes. A transient hold is one only once the run of transient holds (any mix of the
// transient reasons) has lasted TransientHoldAnnounceAfter, and once per run; the run ends with any decision
// that is not a transient hold.
func (r *Runner) announceHold(d, previous Decision, known bool) bool {
	if !transientReason(d.Reason) {
		return !known || previous.Action != ActionHold || previous.Reason != d.Reason
	}
	run, ok := r.quiet[d.Tool]
	if !ok {
		run = quietRun{since: d.At}
		// The run began earlier if the journal's last decision (before a restart) was a transient hold too, but
		// the restart is not worth carrying: a fresh run is only ever later, never an extra event.
	}
	announce := !run.announced && d.At.Sub(run.since) >= TransientHoldAnnounceAfter
	if announce {
		run.announced = true
	}
	r.quiet[d.Tool] = run
	return announce
}

func (r *Runner) event(kind string, payload map[string]string) {
	if r.ev != nil {
		r.ev.Event(kind, payload)
	}
}

// Evaluate runs one pass over every tool.
func (r *Runner) Evaluate(ctx context.Context) {
	r.evalMu.Lock()
	defer r.evalMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tool := range Tools() {
		if ctx.Err() != nil {
			return
		}
		if err := r.evaluateTool(ctx, tool); err != nil {
			log.Printf("quota: %s: %v", tool, err)
			report("checking a tool", false)
		}
	}
}

// okTrack follows one profile's run of ok readings in the usage reader's own time:
// since is the first ok reading's timestamp and last the newest. Re-reading the same
// snapshot does not move last, so a stalled usage poller cannot age a profile into
// "warmed up".
type okTrack struct{ since, last time.Time }

// goodRead is the last ok reading of a profile and when the runner saw it (the reading's own time, else the pass's).
type goodRead struct {
	obs Obs
	at  time.Time
}

// maxBridge caps how long a transient read failure of the usage poller is bridged with the last ok reading.
const maxBridge = 5 * time.Minute

// transientRead says whether a reading is a failed poll that kept the previous numbers (the usage endpoint
// answered 429/5xx or timed out, or the stored credentials were briefly unreadable), as opposed to a login
// that is really unusable (auth_missing, token_expired, reauth...) or a snapshot of a login just swapped in
// (refreshing), which are never bridged.
func transientRead(o Obs) bool {
	switch o.Status {
	case "fetch_error", "timeout", "backing_off", "read_error", "stale_cache":
		return true
	}
	return o.Status == "ok" && o.Stale
}

// track fills in OKFor for every profile and marks a reading older than maxAge stale. A single failed poll
// between two ok readings does not reset the profile's run of ok readings: while the last ok reading is
// younger than the bridge (the maximum age, at most maxBridge) it stands in for the failed one, so the
// numbers the policy acts on are never older than that, and the warm-up is not restarted by every hiccup.
func (r *Runner) track(tool string, profiles []Obs, now time.Time, maxAge time.Duration) {
	bridge := maxBridge
	if maxAge > 0 && maxAge < bridge {
		bridge = maxAge
	}
	for i := range profiles {
		key := tool + "/" + profiles[i].Name
		if profiles[i].OK() {
			at := now
			if profiles[i].ObservedAt != nil {
				at = *profiles[i].ObservedAt
			}
			r.good[key] = goodRead{obs: profiles[i], at: at}
		} else if g, ok := r.good[key]; ok && transientRead(profiles[i]) && now.Sub(g.at) <= bridge {
			cur := profiles[i]
			profiles[i] = g.obs
			profiles[i].Parked, profiles[i].HasCredentials, profiles[i].TokenExpiresAt, profiles[i].Limited = cur.Parked, cur.HasCredentials, cur.TokenExpiresAt, cur.Limited
		}
	}
	AgeObs(profiles, now, maxAge)
	seen := map[string]bool{}
	for i := range profiles {
		key := tool + "/" + profiles[i].Name
		seen[key] = true
		if !profiles[i].OK() {
			delete(r.okSince, key)
			profiles[i].OKFor = 0
			continue
		}
		at := now
		if profiles[i].ObservedAt != nil {
			at = *profiles[i].ObservedAt
		}
		t, ok := r.okSince[key]
		if !ok {
			t = okTrack{since: at, last: at}
		} else if at.After(t.last) {
			t.last = at
		}
		r.okSince[key] = t
		profiles[i].OKFor = t.last.Sub(t.since)
	}
	for key := range r.okSince {
		if len(key) > len(tool) && key[:len(tool)+1] == tool+"/" && !seen[key] {
			delete(r.okSince, key)
			delete(r.good, key)
		}
	}
}

func (r *Runner) evaluateTool(ctx context.Context, tool string) error {
	cfg, _, err := r.loadConfig(ctx, tool)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		r.mem[tool] = memState{}
		return nil
	}
	if r.guard != nil {
		cfg = ClampToSoft(cfg, r.guard(ctx, tool))
	}
	state, err := r.loadState(ctx, tool)
	if err != nil {
		return err
	}
	now := r.now()
	obs, err := r.src.Observe(ctx, tool)
	if err != nil {
		return err
	}
	fixed := func(action, reason string) {
		d := Decision{At: now, Tool: tool, Action: action, Reason: reason, DryRun: cfg.DryRun, Profiles: map[string]ProfileView{}}
		if obs.Active != "" {
			a := obs.Active
			d.From = &a
		}
		at := now
		hold := ""
		if action == ActionHold {
			hold = reason
		}
		r.mem[tool] = memState{Hold: hold, EvaluatedAt: &at}
		if last, ok := r.last[tool]; !ok || !last.Same(d) || r.longTransient(d) {
			r.record(ctx, d)
		}
	}
	if !obs.Ready {
		fixed(ActionHold, ReasonNoSnapshot)
		return nil
	}
	if !obs.SwitcherEnabled {
		fixed(ActionHold, ReasonSwitcherOff)
		return nil
	}
	r.track(tool, obs.Profiles, now, obs.MaxAge)
	if b := r.backoff[tool]; now.Before(b.next) {
		return nil
	}

	res := Decide(Input{Tool: tool, Config: cfg, State: state.State, Profiles: obs.Profiles, Active: obs.Active, Now: now, Migrated: obs.Migrated})
	d := res.Decision
	next := persisted{State: res.State, RecentSwitches: state.RecentSwitches}
	if state.ResetLatchedAt == nil && res.State.ResetLatchedAt != nil && cfg.AfterReset != nil {
		r.event("quota_reset_seen", map[string]string{"tool": tool, "profile": cfg.AfterReset.Profile, "reason": ReasonResetSeen})
	}

	finish := func(final Decision, np persisted) {
		at := now
		hold := ""
		if final.Action == ActionHold {
			hold = final.Reason
		}
		r.mem[tool] = memState{Hold: hold, EvaluatedAt: &at}
		if last, ok := r.last[tool]; !ok || !last.Same(final) || r.longTransient(final) {
			r.record(ctx, final)
		}
		if durableChanged(state, np) {
			if err := r.saveState(ctx, tool, np); err != nil {
				log.Printf("quota: save %s state: %v", tool, err)
				report("saving the switcher state", false)
			}
		}
	}

	if d.Action != ActionSwitch {
		delete(r.backoff, tool)
		finish(d, next)
		return nil
	}
	if cfg.DryRun {
		finish(d, next) // the decision says dry_run; nothing is switched, nothing is counted
		return nil
	}

	to := *d.To
	from := ""
	if d.From != nil {
		from = *d.From
	}
	// The breaker: a fifth switch attempt in an hour (a switch or a failed attempt) is refused and trips the automation.
	recent := next.RecentSwitches[:0:0]
	for _, at := range next.RecentSwitches {
		if now.Sub(at) < breakerWindow {
			recent = append(recent, at)
		}
	}
	next.RecentSwitches = recent
	if len(recent) >= BreakerSwitches {
		next.Tripped = true
		at := now
		next.TrippedAt = &at
		d.Action, d.Reason = ActionHold, ReasonBreaker
		next.Hold = ReasonBreaker
		r.event("quota_switcher_tripped", map[string]string{"tool": tool, "reason": ReasonBreaker})
		finish(d, next)
		return nil
	}

	// The switch can wait on the account lock or a refresh for minutes: let the API
	// read meanwhile (writes wait on evalMu).
	var hold string
	var switched bool
	r.unlocked(func() { switched, hold, err = r.act.Switch(ctx, tool, from, to) })
	switch {
	case hold != "":
		d.Action, d.Reason = ActionHold, hold
		r.bump(tool, now)
		finish(d, next)
	case err != nil:
		log.Printf("quota: switch %s %s -> %s: %v", tool, from, to, err)
		report("automatic account switch failed", false)
		d.Action, d.Reason = ActionError, ReasonSwitchFailed
		b := r.backoff[tool]
		announce := b.failures == 0 || b.lastErr != err.Error()
		r.bump(tool, now)
		if announce {
			// Only a first or a changed failure is announced: a failure that persists would otherwise
			// repeat the event every five minutes for as long as it lasts.
			b = r.backoff[tool]
			b.lastErr = err.Error()
			r.backoff[tool] = b
			r.event("quota_switch_failed", map[string]string{"tool": tool, "from": from, "to": to, "reason": ReasonSwitchFailed})
		}
		// A failed attempt counts toward the breaker like a switch does, so a failure that never goes
		// away trips it instead of retrying for ever.
		next.RecentSwitches = append(next.RecentSwitches, now)
		finish(d, next)
	case !switched:
		// Someone else (the owner, a login) already put the target in place: nothing was switched, so
		// neither the dwell nor the breaker moves and no switch event is raised.
		delete(r.backoff, tool)
		d.Action, d.Reason = ActionStay, ReasonActiveIsBest
		finish(d, next)
	default:
		delete(r.backoff, tool)
		at := now
		next.LastSwitchAt = &at
		next.RecentSwitches = append(next.RecentSwitches, now)
		r.event("quota_switch", map[string]string{"tool": tool, "from": from, "to": to, "reason": d.Reason})
		finish(d, next)
	}
	return nil
}

// unlocked runs fn with r.mu released and takes it again afterwards, also when fn panics: Evaluate
// holds r.mu through a deferred Unlock, so a panic that left it released would turn into a fatal
// "unlock of unlocked mutex" instead of an ordinary, recoverable panic.
func (r *Runner) unlocked(fn func()) {
	r.mu.Unlock()
	defer r.mu.Lock()
	fn()
}

// backoffSteps are the delays after the first, second, third and later failed or
// held switches: 30 s, 1 min, 2 min, then 5 min.
var backoffSteps = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, maxBackoff}

// bump backs a tool off after a failed or held switch.
func (r *Runner) bump(tool string, now time.Time) {
	b := r.backoff[tool]
	b.failures++
	step := b.failures - 1
	if step >= len(backoffSteps) {
		step = len(backoffSteps) - 1
	}
	b.next = now.Add(backoffSteps[step])
	r.backoff[tool] = b
}

func durableChanged(old, next persisted) bool {
	a, _ := json.Marshal(durable(old))
	b, _ := json.Marshal(durable(next))
	return string(a) != string(b)
}

func durable(p persisted) persisted {
	p.Hold, p.EvaluatedAt = "", nil
	return p
}

// ── the API's view ──────────────────────────────────────────────────────────

// ToolView is one tool as the API shows it.
type ToolView struct {
	Config       Config     `json:"config"`
	Saved        bool       `json:"saved"`
	State        State      `json:"state"`
	LastDecision *Decision  `json:"last_decision"`
	Decisions    []Decision `json:"decisions"`
}

// Overview is GET /api/quota-switcher.
type Overview struct {
	Profiles map[string][]string `json:"profiles"`
	Tools    map[string]ToolView `json:"tools"`
}

// ErrUnknownTool is returned for a tool the switcher does not manage.
var ErrUnknownTool = errors.New("unknown tool")

func validTool(tool string) bool { return tool == ToolClaude || tool == ToolCodex }

// Tool returns one tool's view.
func (r *Runner) Tool(ctx context.Context, tool string) (ToolView, error) {
	if !validTool(tool) {
		return ToolView{}, ErrUnknownTool
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.view(ctx, tool)
}

func (r *Runner) view(ctx context.Context, tool string) (ToolView, error) {
	cfg, saved, err := r.loadConfig(ctx, tool)
	if err != nil {
		return ToolView{}, err
	}
	state, err := r.loadState(ctx, tool)
	if err != nil {
		return ToolView{}, err
	}
	state.Hold, state.EvaluatedAt = r.mem[tool].Hold, r.mem[tool].EvaluatedAt
	docs, err := r.store.QuotaDecisions(ctx, tool, 50)
	if err != nil {
		return ToolView{}, err
	}
	view := ToolView{Config: cfg, Saved: saved, State: state.State, Decisions: []Decision{}}
	for _, doc := range docs {
		var d Decision
		if json.Unmarshal([]byte(doc), &d) != nil {
			continue
		}
		view.Decisions = append(view.Decisions, d)
	}
	if len(view.Decisions) > 0 {
		view.LastDecision = &view.Decisions[0]
	}
	return view, nil
}

// Overview returns both tools and the profile names the lists may use.
func (r *Runner) Overview(ctx context.Context) (Overview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Overview{Profiles: map[string][]string{}, Tools: map[string]ToolView{}}
	for _, tool := range Tools() {
		names, err := r.src.Profiles(ctx, tool)
		if err != nil {
			return Overview{}, err
		}
		out.Profiles[tool] = names
		v, err := r.view(ctx, tool)
		if err != nil {
			return Overview{}, err
		}
		out.Tools[tool] = v
	}
	return out, nil
}

// SaveConfig validates and stores tool's configuration and asks for an evaluation.
// Watching a different profile for the reset (or none) forgets the latch and the
// maximum seen, which belonged to the old one.
func (r *Runner) SaveConfig(ctx context.Context, tool string, cfg Config) (ToolView, error) {
	if !validTool(tool) {
		return ToolView{}, ErrUnknownTool
	}
	cfg.Normalize()
	r.evalMu.Lock()
	defer r.evalMu.Unlock()
	r.mu.Lock()
	known, err := r.src.Profiles(ctx, tool)
	if err != nil {
		r.mu.Unlock()
		return ToolView{}, err
	}
	if err := cfg.Validate(known); err != nil {
		r.mu.Unlock()
		return ToolView{}, err
	}
	old, _, err := r.loadConfig(ctx, tool)
	if err != nil {
		r.mu.Unlock()
		return ToolView{}, err
	}
	raw, err := json.Marshal(cfg)
	if err == nil {
		err = r.store.SaveQuotaConfig(ctx, tool, string(raw))
	}
	if err != nil {
		r.mu.Unlock()
		return ToolView{}, err
	}
	if watched(old) != watched(cfg) {
		if state, err := r.loadState(ctx, tool); err == nil && (state.ResetLatchedAt != nil || state.WeeklyMaxSeen != nil) {
			state.ResetLatchedAt, state.WeeklyMaxSeen = nil, nil
			_ = r.saveState(ctx, tool, state)
		}
	}
	delete(r.backoff, tool) // a new configuration is tried at once
	delete(r.last, tool)
	view, err := r.view(ctx, tool)
	r.mu.Unlock()
	r.Trigger()
	return view, err
}

func watched(c Config) string {
	if c.AfterReset == nil {
		return ""
	}
	return c.AfterReset.Profile
}

// Rearm clears the reset latch and the maximum seen, by hand.
func (r *Runner) Rearm(ctx context.Context, tool string) (ToolView, error) {
	return r.clear(ctx, tool, ReasonRearmed, func(p *persisted) {
		p.ResetLatchedAt, p.WeeklyMaxSeen = nil, nil
	})
}

// Reenable clears the circuit breaker (and the count that tripped it).
func (r *Runner) Reenable(ctx context.Context, tool string) (ToolView, error) {
	return r.clear(ctx, tool, ReasonReenabled, func(p *persisted) {
		p.Tripped, p.TrippedAt, p.RecentSwitches = false, nil, nil
	})
}

func (r *Runner) clear(ctx context.Context, tool, reason string, f func(*persisted)) (ToolView, error) {
	if !validTool(tool) {
		return ToolView{}, ErrUnknownTool
	}
	r.evalMu.Lock()
	defer r.evalMu.Unlock()
	r.mu.Lock()
	state, err := r.loadState(ctx, tool)
	if err == nil {
		f(&state)
		err = r.saveState(ctx, tool, state)
	}
	if err != nil {
		r.mu.Unlock()
		return ToolView{}, err
	}
	cfg, _, _ := r.loadConfig(ctx, tool)
	d := Decision{At: r.now(), Tool: tool, Action: ActionStay, Reason: reason, DryRun: cfg.DryRun, Profiles: map[string]ProfileView{}}
	r.record(ctx, d)
	delete(r.backoff, tool)
	view, err := r.view(ctx, tool)
	r.mu.Unlock()
	r.Trigger()
	return view, err
}
