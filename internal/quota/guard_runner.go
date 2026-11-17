package quota

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// GuardStore keeps the guard's per-tool settings as opaque documents.
type GuardStore interface {
	GlobalSettingValue(ctx context.Context, key string) (string, bool, error)
	SetGlobalSettingValue(ctx context.Context, key, value string) error
}

// HoldSetter is where the guard's soft verdict takes effect: while a tool is held the
// dispatcher starts no new turn on it. An empty reason releases it.
type HoldSetter interface {
	SetToolHold(tool, reason string)
}

// guardTick is how often the guard re-reads the usage snapshot's active profile.
const guardTick = 20 * time.Second

// Reasons the guard gives for a hold.
const (
	ReasonGuardSoft = "approaching the usage limit"
	ReasonGuardHard = "at the usage limit"
)

func guardKey(tool string) string { return "limit_guard." + tool }

// Guard keeps each tool's active profile below its soft and hard thresholds: it holds
// new turns at the soft one and tells the owner, and leaves the move to the quota switcher, whose
// limits are clamped to the soft threshold (see ClampToSoft), so it moves as soon as another profile
// is usable. One goroutine (Run) judges;
// the API methods read and write the stored settings.
type Guard struct {
	store GuardStore
	src   Source
	hold  HoldSetter
	ev    Events
	now   func() time.Time
	// CanSwitch reports whether the quota switcher could move tool off its active profile now; it
	// only words the owner's notice ("switching" or "held"). Nil means no.
	CanSwitch func(ctx context.Context, tool string, obs Observation, g GuardConfig) bool
	// OnHold is called when a tool enters the soft or hard state, so the switcher decides at
	// once instead of at its next tick. May be nil.
	OnHold func()
	// OnHoldChange reports every held/unheld edge, including a release on an
	// unclear reading that intentionally produces no owner notice. May be nil.
	OnHoldChange func(tool string, held bool)

	trigger chan struct{}
	// evalMu serialises everything that judges and sets a hold: an evaluation pass and Save, so a concurrent Save and
	// Run cannot leave a hold from a verdict that the new settings have already replaced. mu guards the maps below.
	evalMu sync.Mutex
	mu     sync.Mutex
	view   map[string]GuardView
	since  map[string]time.Time
	// announced is the last definite state told to the owner; an unclear reading changes nothing here,
	// so a flapping snapshot does not repeat a notice.
	announced map[string]string
}

// GuardView is what the web shows of one tool.
type GuardView struct {
	Config GuardConfig `json:"config"`
	// Saved is false while the tool runs on the defaults.
	Saved  bool        `json:"saved"`
	Result GuardResult `json:"result"`
	// Held reports whether new turns are held now.
	Held bool `json:"held"`
	// Since is when the current state began, nil while ok, off or unknown.
	Since *time.Time `json:"since"`
}

// NewGuard builds a guard. ev and hold may be nil.
func NewGuard(store GuardStore, src Source, hold HoldSetter, ev Events) *Guard {
	return &Guard{store: store, src: src, hold: hold, ev: ev, now: time.Now, trigger: make(chan struct{}, 1),
		view: map[string]GuardView{}, since: map[string]time.Time{}, announced: map[string]string{}}
}

// SetClock replaces the clock (tests).
func (g *Guard) SetClock(now func() time.Time) { g.now = now }

// Trigger asks for an evaluation soon.
func (g *Guard) Trigger() {
	select {
	case g.trigger <- struct{}{}:
	default:
	}
}

// Run evaluates on every trigger and tick until ctx ends. It blocks.
func (g *Guard) Run(ctx context.Context) {
	ticker := time.NewTicker(guardTick)
	defer ticker.Stop()
	g.Evaluate(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.trigger:
		case <-ticker.C:
		}
		g.Evaluate(ctx)
	}
}

// Config returns tool's settings and whether any were saved.
func (g *Guard) Config(ctx context.Context, tool string) (GuardConfig, bool, error) {
	doc, found, err := g.store.GlobalSettingValue(ctx, guardKey(tool))
	if err != nil {
		return GuardConfig{}, false, err
	}
	if !found {
		return DefaultGuard(tool), false, nil
	}
	var cfg GuardConfig
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		// A damaged document must not switch the guard off silently or break the switcher.
		log.Printf("quota: stored %s limit guard: %v; using the defaults", tool, err)
		report("the stored limit guard is unreadable; using the defaults", true)
		return DefaultGuard(tool), false, nil
	}
	cfg.Normalize()
	return cfg, true, nil
}

// ConfigOrDefault is Config for callers that cannot act on an error (the runner's clamp):
// a read failure is the defaults.
func (g *Guard) ConfigOrDefault(ctx context.Context, tool string) GuardConfig {
	cfg, _, err := g.Config(ctx, tool)
	if err != nil {
		log.Printf("quota: read %s limit guard: %v", tool, err)
		report("reading the limit guard", false)
		return DefaultGuard(tool)
	}
	return cfg
}

// Save validates and stores tool's settings, then judges again at once.
func (g *Guard) Save(ctx context.Context, tool string, cfg GuardConfig) (GuardView, error) {
	if !validTool(tool) {
		return GuardView{}, ErrUnknownTool
	}
	cfg.Normalize()
	known, err := g.src.Profiles(ctx, tool)
	if err != nil {
		return GuardView{}, err
	}
	if err := cfg.Validate(known); err != nil {
		return GuardView{}, err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return GuardView{}, err
	}
	g.evalMu.Lock()
	defer g.evalMu.Unlock()
	if err := g.store.SetGlobalSettingValue(ctx, guardKey(tool), string(doc)); err != nil {
		return GuardView{}, err
	}
	g.evaluateAll(ctx)
	return g.View(ctx, tool)
}

// View is tool's settings and its last verdict.
func (g *Guard) View(ctx context.Context, tool string) (GuardView, error) {
	if !validTool(tool) {
		return GuardView{}, ErrUnknownTool
	}
	cfg, saved, err := g.Config(ctx, tool)
	if err != nil {
		return GuardView{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	v := g.view[tool]
	v.Config, v.Saved = cfg, saved
	if v.Result.State == "" {
		v.Result = GuardResult{State: GuardUnknown, Soft: cfg.Soft, Hard: cfg.Hard}
	}
	return v, nil
}

// Overview is View of every tool.
func (g *Guard) Overview(ctx context.Context) (map[string]GuardView, error) {
	out := map[string]GuardView{}
	for _, tool := range Tools() {
		v, err := g.View(ctx, tool)
		if err != nil {
			return nil, err
		}
		out[tool] = v
	}
	return out, nil
}

// Evaluate judges every tool once.
func (g *Guard) Evaluate(ctx context.Context) {
	g.evalMu.Lock()
	defer g.evalMu.Unlock()
	g.evaluateAll(ctx)
}

// evaluateAll is Evaluate for a caller that holds evalMu.
func (g *Guard) evaluateAll(ctx context.Context) {
	for _, tool := range Tools() {
		if err := g.evaluateTool(ctx, tool); err != nil && ctx.Err() == nil {
			log.Printf("quota: limit guard %s: %v", tool, err)
			report("limit guard pass", false)
		}
	}
}

func (g *Guard) evaluateTool(ctx context.Context, tool string) error {
	cfg, _, err := g.Config(ctx, tool)
	if err != nil {
		return err
	}
	obs, err := g.src.Observe(ctx, tool)
	if err != nil {
		// No snapshot is unclear data: release rather than hold on a stale verdict.
		g.apply(ctx, tool, cfg, GuardResult{State: GuardUnknown, Soft: cfg.Soft, Hard: cfg.Hard}, Observation{})
		return err
	}
	// The same ageing the runner applies: a stalled poller must not hold turns on old percentages.
	AgeObs(obs.Profiles, g.now(), obs.MaxAge)
	if obs.Solo != nil {
		solo := []Obs{*obs.Solo}
		AgeObs(solo, g.now(), obs.MaxAge)
		obs.Solo = &solo[0]
	}
	active := Obs{}
	for _, o := range obs.Profiles {
		if o.Name == obs.Active {
			active = o
		}
	}
	// A tool with no stored profile has only the implicit one, which still deserves protecting.
	if active.Name == "" && obs.Solo != nil {
		active = *obs.Solo
	}
	g.apply(ctx, tool, cfg, EvaluateGuard(cfg, active), obs)
	return nil
}

func (g *Guard) apply(ctx context.Context, tool string, cfg GuardConfig, res GuardResult, obs Observation) {
	held := res.State == GuardSoft || res.State == GuardHard
	reason := ""
	switch res.State {
	case GuardSoft:
		reason = ReasonGuardSoft
	case GuardHard:
		reason = ReasonGuardHard
	}
	if g.hold != nil {
		g.hold.SetToolHold(tool, reason)
	}
	g.mu.Lock()
	prevView := g.view[tool].Result.State
	prevHeld := g.view[tool].Held
	prev := g.announced[tool]
	now := g.now()
	if res.State != GuardUnknown {
		g.announced[tool] = res.State
	}
	if res.State != prevView {
		if held {
			g.since[tool] = now
		} else {
			delete(g.since, tool)
		}
	}
	v := GuardView{Config: cfg, Result: res, Held: held}
	if at, ok := g.since[tool]; ok && held {
		v.Since = &at
	}
	g.view[tool] = v
	g.mu.Unlock()
	if prevHeld != held && g.OnHoldChange != nil {
		g.OnHoldChange(tool, held)
	}
	if res.State == GuardUnknown || res.State == prev || g.ev == nil {
		return
	}
	g.announce(ctx, tool, prev, res, obs, cfg)
}

// announce reports a change of state to the owner. Only edges are announced, never a repeat.
func (g *Guard) announce(ctx context.Context, tool, prev string, res GuardResult, obs Observation, cfg GuardConfig) {
	percent := ""
	if res.Percent != nil {
		percent = fmt.Sprintf("%.0f", *res.Percent)
	}
	payload := map[string]string{"tool": tool, "profile": res.Profile, "percent": percent, "window": res.Window,
		"soft": fmt.Sprintf("%.0f", res.Soft), "hard": fmt.Sprintf("%.0f", res.Hard)}
	next := "held"
	if g.CanSwitch != nil && res.State != GuardOK && res.State != GuardOff && g.CanSwitch(ctx, tool, obs, cfg) {
		next = "switching"
	}
	payload["next"] = next
	switch res.State {
	case GuardSoft:
		if prev == GuardHard {
			return // the hard notice already said new turns are held
		}
		g.ev.Event("limit_guard_soft", payload)
		if g.OnHold != nil {
			g.OnHold()
		}
	case GuardHard:
		g.ev.Event("limit_guard_hard", payload)
		if g.OnHold != nil {
			g.OnHold()
		}
	case GuardOK, GuardOff:
		delete(payload, "next")
		if prev == GuardSoft || prev == GuardHard {
			g.ev.Event("limit_guard_clear", payload)
		}
	}
}
