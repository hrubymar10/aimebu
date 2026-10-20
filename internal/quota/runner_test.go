package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakes ─────────────────────────────────────────────────────────────────────

type memStore struct {
	mu        sync.Mutex
	config    map[string]string
	state     map[string]string
	decisions map[string][]string // newest first
}

func newMemStore() *memStore {
	return &memStore{config: map[string]string{}, state: map[string]string{}, decisions: map[string][]string{}}
}

func (m *memStore) QuotaConfig(_ context.Context, tool string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.config[tool]
	return d, ok, nil
}
func (m *memStore) SaveQuotaConfig(_ context.Context, tool, doc string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config[tool] = doc
	return nil
}
func (m *memStore) QuotaState(_ context.Context, tool string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.state[tool]
	return d, ok, nil
}
func (m *memStore) SaveQuotaState(_ context.Context, tool, doc string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[tool] = doc
	return nil
}
func (m *memStore) AppendQuotaDecision(_ context.Context, tool, _ string, doc string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions[tool] = append([]string{doc}, m.decisions[tool]...)
	if len(m.decisions[tool]) > 50 {
		m.decisions[tool] = m.decisions[tool][:50]
	}
	return nil
}
func (m *memStore) QuotaDecisions(_ context.Context, tool string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.decisions[tool]
	if len(d) > limit {
		d = d[:limit]
	}
	return append([]string{}, d...), nil
}

type fakeSource struct {
	mu       sync.Mutex
	obs      map[string]Observation
	names    map[string][]string
	observed int
}

func (s *fakeSource) Observe(_ context.Context, tool string) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed++
	o := s.obs[tool]
	o.Profiles = append([]Obs(nil), o.Profiles...)
	return o, nil
}
func (s *fakeSource) Profiles(_ context.Context, tool string) ([]string, error) {
	return s.names[tool], nil
}
func (s *fakeSource) set(tool string, o Observation) {
	s.mu.Lock()
	s.obs[tool] = o
	s.mu.Unlock()
}

type fakeActuator struct {
	calls [][2]string
	froms []string
	hold  string
	err   error
	noop  bool // the target was already active: nothing changed
}

func (a *fakeActuator) Switch(_ context.Context, tool, from, to string) (bool, string, error) {
	a.calls = append(a.calls, [2]string{tool, to})
	a.froms = append(a.froms, from)
	return a.hold == "" && a.err == nil && !a.noop, a.hold, a.err
}

type fakeEvents struct{ events []map[string]string }

func (e *fakeEvents) Event(kind string, payload map[string]string) {
	p := map[string]string{"kind": kind}
	for k, v := range payload {
		p[k] = v
	}
	e.events = append(e.events, p)
}

func (e *fakeEvents) kinds() []string {
	var out []string
	for _, ev := range e.events {
		out = append(out, ev["kind"])
	}
	return out
}

type harness struct {
	t     *testing.T
	store *memStore
	src   *fakeSource
	act   *fakeActuator
	ev    *fakeEvents
	r     *Runner
	now   time.Time
}

// newHarness: claude with profiles a (best), b, c; the active one is b and a is
// usable, so a switch b -> a is wanted. Dry run is off, dwell and warm-up are off
// unless a test turns them on.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: newMemStore(), act: &fakeActuator{}, ev: &fakeEvents{}, now: t0}
	h.src = &fakeSource{obs: map[string]Observation{}, names: map[string][]string{ToolClaude: {"a", "b", "c"}, ToolCodex: {"a", "b"}}}
	h.r = NewRunner(h.store, h.src, h.act, h.ev)
	h.r.SetClock(func() time.Time { return h.now })
	cfg := claudeCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	h.save(ToolClaude, cfg)
	h.observe("b", ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	return h
}

func (h *harness) save(tool string, cfg Config) {
	h.t.Helper()
	if _, err := h.r.SaveConfig(context.Background(), tool, cfg); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) observe(active string, profiles ...Obs) {
	h.src.set(ToolClaude, Observation{Profiles: profiles, Active: active, Migrated: true, SwitcherEnabled: true, Ready: true})
}

func (h *harness) tick(d time.Duration) {
	h.now = h.now.Add(d)
	h.r.Evaluate(context.Background())
}

func (h *harness) view() ToolView {
	h.t.Helper()
	v, err := h.r.Tool(context.Background(), ToolClaude)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) lastReason() (string, string) {
	v := h.view()
	if v.LastDecision == nil {
		h.t.Fatal("no decision recorded")
	}
	return v.LastDecision.Action, v.LastDecision.Reason
}

// tests ─────────────────────────────────────────────────────────────────────

func TestADryRunRecordsTheDecisionAndNeverSwitches(t *testing.T) {
	h := newHarness(t)
	cfg := claudeCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes, cfg.DryRun = 0, 0, true
	h.save(ToolClaude, cfg)
	h.tick(0)
	h.tick(time.Minute)
	if len(h.act.calls) != 0 {
		t.Fatalf("a dry run switched: %v", h.act.calls)
	}
	v := h.view()
	if v.LastDecision == nil || v.LastDecision.Action != ActionSwitch || !v.LastDecision.DryRun || *v.LastDecision.To != "a" {
		t.Fatalf("last decision = %+v", v.LastDecision)
	}
	if len(v.Decisions) != 1 {
		t.Fatalf("an unchanged decision is recorded once, got %d", len(v.Decisions))
	}
	if len(h.ev.events) != 0 || v.State.LastSwitchAt != nil {
		t.Fatalf("a dry run counts and announces nothing: %v %+v", h.ev.events, v.State)
	}
}

func TestARealSwitchIsActedOnceRecordedAnnouncedAndPersisted(t *testing.T) {
	h := newHarness(t)
	h.tick(0)
	if len(h.act.calls) != 1 || h.act.calls[0] != [2]string{ToolClaude, "a"} {
		t.Fatalf("calls = %v", h.act.calls)
	}
	action, reason := h.lastReason()
	if action != ActionSwitch || reason != ReasonHigherUsable {
		t.Fatalf("decision = %s %s", action, reason)
	}
	if got := h.ev.kinds(); len(got) != 1 || got[0] != "quota_switch" || h.ev.events[0]["from"] != "b" || h.ev.events[0]["to"] != "a" {
		t.Fatalf("events = %v", h.ev.events)
	}
	if v := h.view(); v.State.LastSwitchAt == nil {
		t.Fatal("the switch time must be persisted")
	}
	// The usage snapshot now says a is active: the next evaluation stays.
	h.observe("a", ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	h.tick(time.Minute)
	if len(h.act.calls) != 1 {
		t.Fatalf("switched again: %v", h.act.calls)
	}
}

func TestDwellStopsASecondSwitchUntilItHasPassed(t *testing.T) {
	h := newHarness(t)
	cfg := claudeCfg()
	cfg.WarmupSeconds = 0
	h.save(ToolClaude, cfg) // dwell 5 min
	h.tick(0)
	h.observe("a", ob("a", 99, 99), ob("b", 1, 1), ob("c", 1, 1)) // a is exhausted now
	h.tick(time.Minute)
	if len(h.act.calls) != 1 {
		t.Fatalf("switched inside the dwell: %v", h.act.calls)
	}
	if _, reason := h.lastReason(); reason != ReasonSwitchedRecent {
		t.Fatalf("reason = %s", reason)
	}
	h.tick(5 * time.Minute)
	if len(h.act.calls) != 2 || h.act.calls[1][1] != "b" {
		t.Fatalf("calls after the dwell = %v", h.act.calls)
	}
}

func TestWarmupCountsContinuousOkTimeAndARestartWaitsAgain(t *testing.T) {
	h := newHarness(t)
	cfg := claudeCfg()
	cfg.MinDwellMinutes = 0 // warm-up 60 s
	h.save(ToolClaude, cfg)
	h.tick(0)
	if len(h.act.calls) != 0 {
		t.Fatal("switched on the first reading")
	}
	if _, reason := h.lastReason(); reason != ReasonWarmingUp {
		t.Fatalf("reason = %s", reason)
	}
	h.tick(59 * time.Second)
	if len(h.act.calls) != 0 {
		t.Fatal("switched at 59 s")
	}
	// The target going non-ok and coming back restarts its clock.
	flap := ob("a", 1, 1)
	flap.Status = "refreshing"
	h.observe("b", flap, ob("b", 1, 1), ob("c", 1, 1))
	h.tick(time.Second) // t = 60 s: a is not ok, so there is nothing to switch to
	h.observe("b", ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	h.tick(time.Second) // t = 61 s: a is ok again, ok_since restarts here
	h.tick(59 * time.Second)
	if len(h.act.calls) != 0 {
		t.Fatalf("a flap must restart the warm-up: %v", h.act.calls)
	}
	h.tick(time.Second) // t = 121 s: 60 s of continuous ok
	if len(h.act.calls) != 1 {
		t.Fatalf("calls after 60 s of continuous ok = %v", h.act.calls)
	}
	// A restarted runner has no ok history: it waits one warm-up again.
	act2 := &fakeActuator{}
	r2 := NewRunner(h.store, h.src, act2, h.ev)
	r2.SetClock(func() time.Time { return h.now })
	h.observe("b", ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	r2.Evaluate(context.Background())
	if len(act2.calls) != 0 {
		t.Fatal("a restart must wait out a warm-up")
	}
}

func TestAHeldSwitchBacksOffThenRetriesAndSucceeds(t *testing.T) {
	h := newHarness(t)
	h.act.hold = ReasonAgentsRunning
	h.tick(0)
	if action, reason := h.lastReason(); action != ActionHold || reason != ReasonAgentsRunning {
		t.Fatalf("decision = %s %s", action, reason)
	}
	if v := h.view(); v.State.Hold != ReasonAgentsRunning {
		t.Fatalf("state.hold = %q", v.State.Hold)
	}
	h.tick(29 * time.Second) // inside the 30 s back-off: not even tried
	if len(h.act.calls) != 1 {
		t.Fatalf("retried inside the back-off: %v", h.act.calls)
	}
	h.tick(time.Second)
	if len(h.act.calls) != 2 {
		t.Fatalf("not retried after 30 s: %v", h.act.calls)
	}
	h.act.hold = ""
	h.tick(time.Minute) // second back-off is 1 min
	if len(h.act.calls) != 3 {
		t.Fatalf("calls = %v", h.act.calls)
	}
	if action, _ := h.lastReason(); action != ActionSwitch {
		t.Fatalf("the retry should have switched, last = %s", action)
	}
	if got := h.ev.kinds(); len(got) != 1 || got[0] != "quota_switch" {
		t.Fatalf("a hold announces nothing; events = %v", got)
	}
}

func TestAFailedSwitchIsAnErrorWithAFixedReasonAndGrowingBackoff(t *testing.T) {
	h := newHarness(t)
	h.act.err = errors.New("secret detail /home/x/.claude/.credentials.json")
	h.tick(0)
	action, reason := h.lastReason()
	if action != ActionError || reason != ReasonSwitchFailed {
		t.Fatalf("decision = %s %s", action, reason)
	}
	if got := h.ev.kinds(); len(got) != 1 || got[0] != "quota_switch_failed" {
		t.Fatalf("events = %v", got)
	}
	for _, ev := range h.ev.events {
		for _, v := range ev {
			if v == h.act.err.Error() {
				t.Fatal("the error text must not reach an event")
			}
		}
	}
	// Back-offs 30 s, 1 min, 2 min (the 5 min cap is exercised by the breaker test below).
	for i, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		before := len(h.act.calls)
		h.tick(wait - time.Second)
		if len(h.act.calls) != before {
			t.Fatalf("step %d: retried %s early", i, time.Second)
		}
		h.tick(time.Second)
		if len(h.act.calls) != before+1 {
			t.Fatalf("step %d: not retried after %s", i, wait)
		}
	}
}

// A failure that persists is announced once (a changed failure is announced again), and the failed attempts
// count toward the breaker: the fifth attempt in an hour trips it instead of failing every five minutes for ever.
func TestAPersistentFailureIsAnnouncedOnceAndEventuallyTripsTheBreaker(t *testing.T) {
	h := newHarness(t)
	h.act.err = errors.New("disk is read-only")
	failed := func() int {
		n := 0
		for _, kind := range h.ev.kinds() {
			if kind == "quota_switch_failed" {
				n++
			}
		}
		return n
	}
	for i := 0; i < BreakerSwitches; i++ {
		h.tick(6 * time.Minute) // past every back-off
	}
	if len(h.act.calls) != BreakerSwitches || failed() != 1 {
		t.Fatalf("%d attempts, %d failure events: the same failure is announced once", len(h.act.calls), failed())
	}
	// A different failure is news.
	h.act.err = errors.New("permission denied")
	h.tick(6 * time.Minute)
	// The fifth attempt in the hour is refused and trips the breaker.
	if len(h.act.calls) != BreakerSwitches {
		t.Fatalf("the breaker must stop the attempts: %d calls", len(h.act.calls))
	}
	st := h.view().State
	if ReasonBreaker != "too many automatic switch attempts this hour" {
		t.Fatalf("the breaker's reason must say attempts, since failed attempts count: %q", ReasonBreaker)
	}
	if !st.Tripped || h.view().State.Hold != ReasonBreaker {
		t.Fatalf("a failure that never goes away must trip the breaker: %+v", st)
	}
	h2 := newHarness(t)
	h2.act.err = errors.New("one")
	h2.tick(0)
	h2.act.err = errors.New("two")
	h2.tick(time.Minute)
	if got := func() int {
		n := 0
		for _, kind := range h2.ev.kinds() {
			if kind == "quota_switch_failed" {
				n++
			}
		}
		return n
	}(); got != 2 {
		t.Fatalf("a changed failure is announced again: %d", got)
	}
}

func TestTheBreakerRefusesTheFifthSwitchInAnHourAndTheOwnerRearmsIt(t *testing.T) {
	h := newHarness(t)
	active := "b"
	// Alternate exhausted/usable so every evaluation wants a switch.
	flip := func() {
		if active == "b" {
			h.observe("b", ob("a", 1, 1), ob("b", 99, 99), ob("c", 99, 99)) // want a
		} else {
			h.observe("a", ob("a", 99, 99), ob("b", 1, 1), ob("c", 99, 99)) // want b
		}
	}
	for i := 0; i < BreakerSwitches; i++ {
		flip()
		h.tick(time.Minute)
		if len(h.act.calls) != i+1 {
			t.Fatalf("switch %d: calls = %v", i+1, h.act.calls)
		}
		if active == "b" {
			active = "a"
		} else {
			active = "b"
		}
	}
	flip()
	h.tick(time.Minute)
	if len(h.act.calls) != BreakerSwitches {
		t.Fatalf("the fifth switch was made: %v", h.act.calls)
	}
	v := h.view()
	if !v.State.Tripped || v.State.TrippedAt == nil {
		t.Fatalf("the breaker did not trip: %+v", v.State)
	}
	if got := h.ev.kinds(); got[len(got)-1] != "quota_switcher_tripped" {
		t.Fatalf("events = %v", got)
	}
	if _, reason := h.lastReason(); reason != ReasonBreaker {
		t.Fatalf("reason = %s", reason)
	}
	h.tick(time.Minute)
	if _, reason := h.lastReason(); reason != ReasonTripped || len(h.act.calls) != BreakerSwitches {
		t.Fatalf("a tripped tool holds: %s %v", reason, h.act.calls)
	}
	// It survives a restart (it is persisted).
	r2 := NewRunner(h.store, h.src, h.act, h.ev)
	r2.SetClock(func() time.Time { return h.now })
	if view, _ := r2.Tool(context.Background(), ToolClaude); !view.State.Tripped {
		t.Fatal("the breaker must be persisted")
	}
	if _, err := h.r.Reenable(context.Background(), ToolClaude); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Minute)
	if len(h.act.calls) != BreakerSwitches+1 {
		t.Fatalf("after re-enabling the switch is made: %v", h.act.calls)
	}
}

func TestSwitchesOlderThanAnHourDoNotCountTowardTheBreaker(t *testing.T) {
	h := newHarness(t)
	active := "b"
	for i := 0; i < BreakerSwitches*2; i++ {
		if active == "b" {
			h.observe("b", ob("a", 1, 1), ob("b", 99, 99), ob("c", 99, 99))
			active = "a"
		} else {
			h.observe("a", ob("a", 99, 99), ob("b", 1, 1), ob("c", 99, 99))
			active = "b"
		}
		h.tick(20 * time.Minute) // three per hour: never the fifth
	}
	if len(h.act.calls) != BreakerSwitches*2 || h.view().State.Tripped {
		t.Fatalf("calls=%d tripped=%v", len(h.act.calls), h.view().State.Tripped)
	}
}

func TestADisabledToolIsNeverObservedOrSwitched(t *testing.T) {
	h := newHarness(t)
	cfg := claudeCfg()
	cfg.Enabled = false
	h.save(ToolClaude, cfg)
	before := h.src.observed
	h.tick(time.Minute)
	if h.src.observed != before || len(h.act.calls) != 0 {
		t.Fatalf("a disabled tool did work: observed %d->%d calls %v", before, h.src.observed, h.act.calls)
	}
}

func TestNoSnapshotAndASwitcherThatIsOffAreHoldsRecordedOnce(t *testing.T) {
	h := newHarness(t)
	h.src.set(ToolClaude, Observation{Ready: false})
	h.tick(0)
	h.tick(time.Minute)
	if _, reason := h.lastReason(); reason != ReasonNoSnapshot || len(h.view().Decisions) != 1 {
		t.Fatalf("no-snapshot hold: %s, %d recorded", reason, len(h.view().Decisions))
	}
	h.src.set(ToolClaude, Observation{Ready: true, SwitcherEnabled: false, Profiles: []Obs{ob("a", 1, 1)}, Active: "a"})
	h.tick(time.Minute)
	if _, reason := h.lastReason(); reason != ReasonSwitcherOff {
		t.Fatalf("switcher-off hold: %s", reason)
	}
	if len(h.act.calls) != 0 {
		t.Fatal("switched with nothing to go on")
	}
}

func TestTheResetLatchPersistsRaisesOneEventAndIsRearmedByHand(t *testing.T) {
	h := newHarness(t)
	cfg := resetCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes, cfg.DryRun = 0, 0, true
	h.save(ToolClaude, cfg)
	h.observe("b", ob("a", 1, 84), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	h.observe("b", ob("a", 1, 64), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	h.tick(time.Second)
	v := h.view()
	if v.State.ResetLatchedAt == nil {
		t.Fatalf("the reset was not latched: %+v", v.State)
	}
	var resets int
	for _, k := range h.ev.kinds() {
		if k == "quota_reset_seen" {
			resets++
		}
	}
	if resets != 1 {
		t.Fatalf("quota_reset_seen events = %d", resets)
	}
	// A restart keeps it.
	r2 := NewRunner(h.store, h.src, h.act, h.ev)
	r2.SetClock(func() time.Time { return h.now })
	if view, _ := r2.Tool(context.Background(), ToolClaude); view.State.ResetLatchedAt == nil {
		t.Fatal("the latch must be persisted")
	}
	// Re-arming is by hand and is recorded.
	view, err := h.r.Rearm(context.Background(), ToolClaude)
	if err != nil || view.State.ResetLatchedAt != nil || view.State.WeeklyMaxSeen != nil {
		t.Fatalf("rearm: %+v %v", view.State, err)
	}
	if view.LastDecision == nil || view.LastDecision.Reason != ReasonRearmed {
		t.Fatalf("the re-arm must be in the history: %+v", view.LastDecision)
	}
}

func TestChangingTheWatchedProfileForgetsTheLatch(t *testing.T) {
	h := newHarness(t)
	cfg := resetCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes, cfg.DryRun = 0, 0, true
	h.save(ToolClaude, cfg)
	h.observe("b", ob("a", 1, 84), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	h.observe("b", ob("a", 1, 40), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	if h.view().State.ResetLatchedAt == nil {
		t.Fatal("setup: the latch should be set")
	}
	cfg.AfterReset.Profile = "c"
	cfg.AfterReset.Profiles = []Limit{{Name: "b", SessionMax: 95, WeeklyMax: 95}, {Name: "c", SessionMax: 95, WeeklyMax: 95}}
	h.save(ToolClaude, cfg)
	if st := h.view().State; st.ResetLatchedAt != nil || st.WeeklyMaxSeen != nil {
		t.Fatalf("a latch for another profile must be forgotten: %+v", st)
	}
}

// With the reset latched the after-reset order replaces the main one; the runner then switches by it.
func TestAfterTheResetLatchesTheRunnerSwitchesByTheNewOrder(t *testing.T) {
	h := newHarness(t)
	cfg := resetCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	h.save(ToolClaude, cfg)
	h.observe("a", ob("a", 1, 84), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	if len(h.act.calls) != 0 {
		t.Fatalf("a is first in the main order and active, nothing to switch: %v", h.act.calls)
	}
	h.observe("a", ob("a", 1, 64), ob("b", 1, 10), ob("c", 1, 10))
	h.tick(time.Second)
	h.tick(time.Second)
	if h.view().State.ResetLatchedAt == nil {
		t.Fatalf("the reset must latch: %+v", h.view().State)
	}
	if len(h.act.calls) == 0 {
		t.Fatal("after the latch the after-reset order puts b first, so the runner must switch to it")
	}
	for _, call := range h.act.calls {
		if call != [2]string{ToolClaude, "b"} {
			t.Fatalf("after the latch the after-reset order puts b first: %v", h.act.calls)
		}
	}
}

// The runner hands the profile it decided from to the actuator, and a hold for a changed active
// profile counts as nothing: no event, no dwell, no breaker, and the next pass decides again.
func TestTheActuatorGetsTheDecidedFromProfileAndAChangedActiveIsOnlyAHold(t *testing.T) {
	h := newHarness(t) // active b, a is wanted
	h.act.hold = ReasonActiveChanged
	h.tick(time.Second)
	if len(h.act.froms) != 1 || h.act.froms[0] != "b" {
		t.Fatalf("the actuator must be told the decision was made from b: %v", h.act.froms)
	}
	if kinds := h.ev.kinds(); len(kinds) != 0 {
		t.Fatalf("a changed active profile raises no event: %v", kinds)
	}
	if st := h.view().State; st.LastSwitchAt != nil {
		t.Fatalf("a held switch moved the dwell or the breaker: %+v", st)
	}
	if action, reason := h.lastReason(); action != ActionHold || reason != ReasonActiveChanged {
		t.Fatalf("decision = %s %q", action, reason)
	}
	// The owner switched to a meanwhile: the next pass sees it and has nothing to do.
	h.act.hold = ""
	h.observe("a", ob("a", 1, 1), ob("b", 1, 1), ob("c", 1, 1))
	h.tick(time.Hour)
	if action, _ := h.lastReason(); action != ActionStay || len(h.act.froms) != 1 {
		t.Fatalf("after the owner's switch the runner must stay: %s, calls %v", action, h.act.froms)
	}
}

// An actuator that finds the target already active switched nothing: no event, no dwell, no breaker count.
func TestASwitchThatChangedNothingIsNotCounted(t *testing.T) {
	h := newHarness(t)
	h.act.noop = true
	for i := 0; i < BreakerSwitches+2; i++ {
		h.tick(time.Minute)
	}
	if len(h.act.calls) == 0 {
		t.Fatal("setup: the actuator was never called")
	}
	for _, kind := range h.ev.kinds() {
		if kind == "quota_switch" || kind == "quota_switcher_tripped" {
			t.Fatalf("a no-op was announced or tripped the breaker: %v", h.ev.kinds())
		}
	}
	st := h.view().State
	if st.LastSwitchAt != nil || st.Tripped {
		t.Fatalf("a no-op moved the dwell or the breaker: %+v", st)
	}
}

type panickingActuator struct{}

func (panickingActuator) Switch(context.Context, string, string, string) (bool, string, error) {
	panic("actuator blew up")
}

// A panic in the actuator, which runs with the runner's mutex released, must stay an ordinary panic: the
// mutex is held again when Evaluate's deferred Unlock runs. (Without it the process dies with a fatal,
// unrecoverable "unlock of unlocked mutex", which this test would not survive.)
func TestAPanicInTheActuatorKeepsTheMutexBalanced(t *testing.T) {
	h := newHarness(t)
	h.r = NewRunner(h.store, h.src, panickingActuator{}, h.ev)
	h.r.SetClock(func() time.Time { return h.now })
	cfg := claudeCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	h.save(ToolClaude, cfg)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the actuator's panic was swallowed")
			}
		}()
		h.tick(time.Second)
	}()
	if !h.r.mu.TryLock() {
		t.Fatal("the runner's mutex is still held after the panic")
	}
	h.r.mu.Unlock()
	if !h.r.evalMu.TryLock() {
		t.Fatal("the evaluation mutex is still held after the panic")
	}
	h.r.evalMu.Unlock()
}

func TestSaveConfigValidatesAgainstTheProfilesThatExist(t *testing.T) {
	h := newHarness(t)
	bad := claudeCfg()
	bad.Profiles[0].Name = "ghost"
	if _, err := h.r.SaveConfig(context.Background(), ToolClaude, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if _, err := h.r.SaveConfig(context.Background(), "pi", claudeCfg()); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	// The stored config is unchanged by a rejected save.
	if got := h.view().Config.Profiles[0].Name; got != "a" {
		t.Fatalf("a rejected save changed the config: %s", got)
	}
}

func TestOverviewPrefillsAnUnsavedToolFromTheExistingProfiles(t *testing.T) {
	h := newHarness(t)
	o, err := h.r.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	codex := o.Tools[ToolCodex]
	if codex.Saved || codex.Config.Enabled || !codex.Config.DryRun || len(codex.Config.Profiles) != 2 ||
		codex.Config.Profiles[0].Name != "a" || codex.Config.Profiles[0].SessionMax != 93 || codex.Config.Profiles[1].SessionMax != 98 {
		t.Fatalf("codex default = %+v", codex)
	}
	if !o.Tools[ToolClaude].Saved || len(o.Profiles[ToolClaude]) != 3 {
		t.Fatalf("claude = %+v", o.Tools[ToolClaude])
	}
	if codex.Decisions == nil || len(codex.Config.Protected) != 0 || codex.Config.Protected == nil {
		t.Fatal("lists must encode as arrays, never null")
	}
}

type blockingActuator struct {
	entered chan struct{}
	release chan struct{}
}

func (a *blockingActuator) Switch(context.Context, string, string, string) (bool, string, error) {
	a.entered <- struct{}{}
	<-a.release
	return true, "", nil
}

// A switch can wait minutes on an account lock or a refresh. The API must keep
// answering reads meanwhile; a write waits for the switch to end, so it cannot be
// overwritten by the state that switch persists.
func TestTheAPIReadsDuringASlowSwitchAndAWriteWaitsForIt(t *testing.T) {
	h := newHarness(t)
	act := &blockingActuator{entered: make(chan struct{}), release: make(chan struct{})}
	h.r = NewRunner(h.store, h.src, act, h.ev)
	h.r.SetClock(func() time.Time { return h.now })
	evaluated := make(chan struct{})
	go func() { h.r.Evaluate(context.Background()); close(evaluated) }()
	select {
	case <-act.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never started")
	}

	read := make(chan error, 1)
	go func() { _, err := h.r.Overview(context.Background()); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read blocked behind the slow switch")
	}

	cfg := claudeCfg()
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	wrote := make(chan error, 1)
	go func() { _, err := h.r.SaveConfig(context.Background(), ToolClaude, cfg); wrote <- err }()
	select {
	case <-wrote:
		t.Fatal("a write went through while a switch was in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(act.release)
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never completed after the switch ended")
	}
	<-evaluated
}

// A default that was never saved is shown pre-filled and is never acted on: it is
// disabled and in dry run, the runner does not even observe the tool, and only an
// explicit save by the owner changes that.
func TestAnUnsavedDefaultIsNeverActedOn(t *testing.T) {
	h := newHarness(t)
	off := claudeCfg()
	off.Enabled = false
	h.save(ToolClaude, off) // leave only codex, which was never saved
	h.src.set(ToolCodex, Observation{Active: "b", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []Obs{ob("a", 1, 1), ob("b", 99, 99)}})
	before := h.src.observed
	h.tick(time.Minute)
	h.tick(time.Minute)
	if len(h.act.calls) != 0 || h.src.observed != before {
		t.Fatalf("an unsaved tool was observed or switched: calls=%v observed=%d", h.act.calls, h.src.observed-before)
	}
	o, err := h.r.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	codex := o.Tools[ToolCodex]
	if codex.Saved || codex.Config.Enabled || !codex.Config.DryRun || len(codex.Config.Protected) != 0 {
		t.Fatalf("the unsaved default must be off, dry-run and protect nothing: %+v", codex)
	}
	// A save that turns it on but leaves dry run on still switches nothing.
	cfg := Default(ToolCodex, []string{"a", "b"})
	cfg.Enabled, cfg.WarmupSeconds, cfg.MinDwellMinutes = true, 0, 0
	if _, err := h.r.SaveConfig(context.Background(), ToolCodex, cfg); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Minute)
	if len(h.act.calls) != 0 {
		t.Fatalf("a dry-run save switched: %v", h.act.calls)
	}
	v, _ := h.r.Tool(context.Background(), ToolCodex)
	if v.LastDecision == nil || v.LastDecision.Action != ActionSwitch || !v.LastDecision.DryRun {
		t.Fatalf("the dry run should have recorded the would-be switch: %+v", v.LastDecision)
	}
}

// ── freshness: a stalled usage poller must not look like settled data ───────

func withReading(o Obs, at time.Time) Obs {
	o.ObservedAt = &at
	return o
}

func TestWarmupIsMeasuredInTheReadersTimeNotByRereadingOneSnapshot(t *testing.T) {
	h := newHarness(t)
	cfg := claudeCfg()
	cfg.MinDwellMinutes = 0 // 60 s warm-up
	h.save(ToolClaude, cfg)
	frozen := h.now
	set := func(at time.Time) {
		h.src.set(ToolClaude, Observation{Active: "b", Migrated: true, SwitcherEnabled: true, Ready: true, MaxAge: 10 * time.Minute,
			Profiles: []Obs{withReading(ob("a", 1, 1), at), withReading(ob("b", 1, 1), at), withReading(ob("c", 1, 1), at)}})
	}
	set(frozen)
	// The poller stalls: the same snapshot is re-read for over ten minutes, ok
	// throughout. It never warms up, and past the maximum age it is stale.
	for i := 0; i < 41; i++ {
		h.tick(15 * time.Second)
	}
	if len(h.act.calls) != 0 {
		t.Fatalf("re-reading one snapshot warmed it up: %v", h.act.calls)
	}
	// Fresh snapshots arrive again, 120 s of the reader's time apart: now it counts.
	set(frozen.Add(10 * time.Minute))
	h.tick(time.Second)
	set(frozen.Add(10*time.Minute + 120*time.Second))
	h.tick(time.Second)
	if len(h.act.calls) != 1 {
		t.Fatalf("with fresh snapshots the warm-up completes: %v", h.act.calls)
	}
}

func TestAReadingOlderThanTheMaximumAgeIsStaleAndHoldsTheTool(t *testing.T) {
	h := newHarness(t)
	old := h.now.Add(-11 * time.Minute)
	fresh := h.now
	h.src.set(ToolClaude, Observation{Active: "b", Migrated: true, SwitcherEnabled: true, Ready: true, MaxAge: 10 * time.Minute,
		Profiles: []Obs{withReading(ob("a", 1, 1), fresh), withReading(ob("b", 1, 1), old), withReading(ob("c", 1, 1), fresh)}})
	h.tick(0)
	if len(h.act.calls) != 0 {
		t.Fatal("acted on a stale active reading")
	}
	v := h.view()
	if v.LastDecision == nil || v.LastDecision.Reason != ReasonActiveNotOK || v.LastDecision.Profiles["b"].Status != "stale" {
		t.Fatalf("decision = %+v", v.LastDecision)
	}
	// Ten minutes exactly is still fine.
	h.src.set(ToolClaude, Observation{Active: "b", Migrated: true, SwitcherEnabled: true, Ready: true, MaxAge: 10 * time.Minute,
		Profiles: []Obs{withReading(ob("a", 1, 1), fresh), withReading(ob("b", 1, 1), h.now.Add(-10*time.Minute)), withReading(ob("c", 1, 1), fresh)}})
	h.tick(0)
	if len(h.act.calls) != 1 {
		t.Fatalf("a reading at the limit must still count: %v", h.act.calls)
	}
}
