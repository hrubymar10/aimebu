package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// kaHarness is a warm harness whose deliveries carry a keep-alive setting and whose pings' reports are collected.
type kaHarness struct {
	*warmHarness
	mu       sync.Mutex
	reports  []protocol.KeepAliveReport
	scopes   []string // "on agt" / "off agt" as the server is told, in order
	scopeErr error    // what the server's answer to a scope request is
	offErr   error    // what the server's answer to a scope lift (only) is
	gens     []string // the ping each scope request named, parallel to scopes
	// started says how many processes had been started when each scope call was made, so a test can tell the scope came before the process.
	started []int
}

func newKAHarness(t *testing.T) *kaHarness {
	t.Helper()
	h := &kaHarness{warmHarness: newWarmHarness(t)}
	h.ka = &protocol.KeepAliveSettings{IntervalSeconds: 3000, StopAfterSeconds: 24 * 3600}
	h.c.Warm = WarmConfig{} // warm mode off: a ping is then a cold resume, which is the usual case (the window is shorter than the interval)
	h.c.keepAliveSink = func(r protocol.KeepAliveReport) { h.mu.Lock(); h.reports = append(h.reports, r); h.mu.Unlock() }
	h.c.keepAliveScope = func(agentID, gen string, on bool) error {
		h.f.mu.Lock()
		cold := h.f.cold
		h.f.mu.Unlock()
		h.mu.Lock()
		defer h.mu.Unlock()
		word := "off "
		if on {
			word = "on "
		}
		h.scopes, h.started = append(h.scopes, word+agentID), append(h.started, cold)
		h.gens = append(h.gens, gen)
		if !on && h.offErr != nil {
			return h.offErr
		}
		return h.scopeErr
	}
	return h
}

func (h *kaHarness) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.reports) }

// fire runs the armed keep-alive timer (the newest one that has not been stopped) and waits for its report.
func (h *kaHarness) fire(t *testing.T) protocol.KeepAliveReport {
	t.Helper()
	before := h.count()
	var armed *fakeTimer
	timers := h.timerList()
	for i := len(timers) - 1; i >= 0; i-- {
		if timers[i].d == time.Duration(h.ka.IntervalSeconds)*time.Second || timers[i].d == keepAliveRetryBusy {
			armed = timers[i]
			break
		}
	}
	if armed == nil {
		t.Fatal("no keep-alive timer is armed")
	}
	armed.f()
	eventually(t, "the ping's report", func() bool { return h.count() > before })
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reports[len(h.reports)-1]
}

// A real turn arms the timer one interval ahead; when it fires the ping is a cold resume with the real turn's own arguments, its user turn marked as
// a keep-alive with no notes, and it reports what it read from the cache. It arms the next one.
func TestKeepAlivePingsAfterTheInterval(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) {
		s.NativeSessionHandle = "sess-1"
		s.ContextJSON = []byte(`{"role_body":"lead"}`)
	})
	if h.f.cold != 1 {
		t.Fatalf("the real turn: cold=%d", h.f.cold)
	}
	armedBefore := len(h.timerList())
	report := h.fire(t)
	if !report.OK || report.CacheRead != 4253 || report.CacheWrite != 177 || report.AgentID != "agt_a" {
		t.Fatalf("report = %+v", report)
	}
	if h.f.cold != 2 {
		t.Fatalf("the ping must start a process of its own: cold=%d", h.f.cold)
	}
	ping := h.f.coldSpecs[1]
	if !ping.KeepAlive || !ping.NativeSessionStarted || ping.NativeSessionHandle != "sess-1" || string(ping.NotesJSON) != "[]" || string(ping.ContextJSON) != `{"role_body":"lead"}` || ping.AgentTools == nil || !strings.HasPrefix(ping.TurnID, "ka_") {
		t.Fatalf("ping spec = %+v", ping)
	}
	if h.f.coldSpecs[0].KeepAlive {
		t.Fatal("the real turn is not a keep-alive")
	}
	// the next ping is armed an interval after this one
	eventually(t, "the next ping being armed", func() bool { return len(h.timerList()) > armedBefore })
	if last := h.timerList(); last[len(last)-1].d != 3000*time.Second {
		t.Fatalf("next timer = %v", last[len(last)-1].d)
	}
}

// A real turn resets the timer: the one armed before it is stopped, and the new one counts from the turn's end.
func TestKeepAliveRealTurnResetsTheTimer(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	first := h.lastTimer()
	h.run("agt_a", nil)
	h.kaStopped(t, first)
	if got := h.lastTimer(); got == first || got.d != 3000*time.Second {
		t.Fatalf("the second turn must arm a new timer: %+v", got)
	}
	// the first timer, fired late, does nothing: it belongs to an earlier arming
	first.f()
	time.Sleep(50 * time.Millisecond)
	if h.count() != 0 || h.f.cold != 2 {
		t.Fatalf("a stale timer pinged: reports=%d cold=%d", h.count(), h.f.cold)
	}
}

// kaStopped checks a timer was stopped: its underlying timer no longer fires (stopping it a second time says so).
func (h *kaHarness) kaStopped(t *testing.T, ft *fakeTimer) {
	t.Helper()
	if ft.timer.Stop() {
		t.Fatal("the timer was left armed")
	}
}

// After the owner's number of hours without real work the pings stop and the agent is let go.
func TestKeepAliveStopsAfterTheLimit(t *testing.T) {
	h := newKAHarness(t)
	h.ka.StopAfterSeconds = 2 * 3600
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.fire(t) // within the limit
	h.now = h.now.Add(3 * time.Hour)
	before := h.count()
	var armed *fakeTimer
	for i := len(h.timerList()) - 1; i >= 0; i-- {
		if h.timers[i].d == 3000*time.Second {
			armed = h.timers[i]
			break
		}
	}
	armed.f()
	time.Sleep(50 * time.Millisecond)
	if h.count() != before {
		t.Fatal("pinged after the limit")
	}
	h.c.kaMu.Lock()
	_, kept := h.c.ka["agt_a"]
	h.c.kaMu.Unlock()
	if kept {
		t.Fatal("the agent is still kept after the limit")
	}
}

// Three failures in a row stop the pings; a success in between starts the count again.
func TestKeepAliveStopsAfterThreeFailures(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.mu.Lock()
	h.f.coldFail = true
	h.f.mu.Unlock()
	for i := 0; i < keepAliveMaxFails; i++ {
		before := len(h.timerList())
		if report := h.fire(t); report.OK || report.Error == "" {
			t.Fatalf("failure %d: %+v", i, report)
		}
		if i < keepAliveMaxFails-1 {
			eventually(t, "the ping being tried again", func() bool { return len(h.timerList()) > before })
		}
	}
	time.Sleep(50 * time.Millisecond)
	n := len(h.timerList())
	time.Sleep(50 * time.Millisecond)
	if len(h.timerList()) != n {
		t.Fatal("pings went on after three failures")
	}
}

// A setting of zero, no setting, a tool the adapter cannot ping, a fork or a correction forgets the agent: nothing is armed.
func TestKeepAliveOnlyWhereItCanRun(t *testing.T) {
	cases := map[string]func(h *kaHarness, s *adapter.TurnSpec){
		"no setting":    func(h *kaHarness, s *adapter.TurnSpec) { h.ka = nil },
		"interval zero": func(h *kaHarness, s *adapter.TurnSpec) { h.ka = &protocol.KeepAliveSettings{} },
		"another tool":  func(h *kaHarness, s *adapter.TurnSpec) { s.Agent.Tool = "codex" },
		"a fork":        func(h *kaHarness, s *adapter.TurnSpec) { s.ForkSession = true },
		"a correction":  func(h *kaHarness, s *adapter.TurnSpec) { s.Mode = adapter.ModeCorrection },
	}
	for name, change := range cases {
		h := newKAHarness(t)
		h.spawn("agt_a")
		h.run("agt_a", func(s *adapter.TurnSpec) { change(h, s) })
		h.c.kaMu.Lock()
		_, kept := h.c.ka["agt_a"]
		h.c.kaMu.Unlock()
		if kept || len(h.timerList()) != 0 {
			t.Errorf("%s: kept=%v timers=%d", name, kept, len(h.timerList()))
		}
	}
}

// A turn that does not end cleanly leaves no ping armed (the agent's last turn needs a decision, and a ping would only run on top of it).
func TestKeepAliveNotArmedAfterAFailedTurn(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.f.mu.Lock()
	h.f.coldFail = true
	h.f.mu.Unlock()
	h.run("agt_a", nil)
	for _, ft := range h.timerList() {
		if ft.d == 3000*time.Second && ft.timer.Stop() {
			t.Fatal("a ping is armed after a failed turn")
		}
	}
}

// A stopped agent is forgotten at once and its pending ping is stopped; a ping whose agent has since been stopped does not run.
func TestKeepAliveStopsWithTheAgent(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	armed := h.lastTimer()
	h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"})
	h.kaStopped(t, armed)
	h.c.kaMu.Lock()
	_, kept := h.c.ka["agt_a"]
	h.c.kaMu.Unlock()
	if kept {
		t.Fatal("a stopped agent is still kept")
	}
	armed.f()
	time.Sleep(50 * time.Millisecond)
	if h.count() != 0 {
		t.Fatal("a stopped agent was pinged")
	}
}

// A real delivery that arrives while a ping runs on this machine ends the ping and goes ahead; the agent is not pinged again until the turn ends.
func TestKeepAliveYieldsToARealDelivery(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	block := make(chan struct{})
	h.f.mu.Lock()
	h.f.coldBlock = block
	h.f.mu.Unlock()
	h.lastTimer().f() // the ping starts and is held in its start
	eventually(t, "the ping starting", func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return h.f.cold == 2 })
	h.c.kaMu.Lock()
	running := h.c.ka["agt_a"].cancel != nil
	h.c.kaMu.Unlock()
	if !running {
		t.Fatal("the ping is not running")
	}
	done := make(chan struct{})
	go func() { h.c.keepAliveYield("agt_a"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a real delivery waited for a ping it could have ended")
	}
	h.c.kaMu.Lock()
	still := h.c.ka["agt_a"].cancel != nil
	h.c.kaMu.Unlock()
	eventually(t, "the ended ping to settle", func() bool { h.c.kaMu.Lock(); defer h.c.kaMu.Unlock(); return h.c.ka["agt_a"].cancel == nil })
	_ = still
}

// A ping never starts a warm process: with warm mode on and no live process it is a cold resume; with one live and idle it goes into it.
func TestKeepAliveNeverStartsAWarmProcess(t *testing.T) {
	h := newKAHarness(t)
	h.c.Warm = WarmConfig{Idle: 5 * time.Minute}
	h.spawn("agt_a")
	h.run("agt_a", nil) // a warm process is started for the real turn and goes idle
	h.c.RetireAllWarm("window over")
	if started := h.f.started; started != 1 {
		t.Fatalf("started = %d", started)
	}
	h.fire(t)
	if h.f.started != 1 || h.f.cold != 1 {
		t.Fatalf("the ping started a warm process: started=%d cold=%d", h.f.started, h.f.cold)
	}
	if h.idle("agt_a") {
		t.Fatal("a ping left a warm process idle")
	}
}

// A ping that runs while the agent is mid-turn (it cannot, but the check is there) is put off, not run.
func TestKeepAliveWaitsForARunningTurn(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.c.mu.Lock()
	h.c.active["agt_a"] = "turn_busy"
	h.c.mu.Unlock()
	h.lastTimer().f()
	time.Sleep(50 * time.Millisecond)
	if h.count() != 0 {
		t.Fatal("pinged while a turn ran")
	}
	if h.lastTimer().d != keepAliveRetryBusy {
		t.Fatalf("it must look again soon: %v", h.lastTimer().d)
	}
}

func (h *warmHarness) lastTimer() *fakeTimer {
	list := h.timerList()
	return list[len(list)-1]
}

// What a ping cost is the session's cumulative cost after it less what it was before; nothing is claimed when either is unknown.
func TestKeepAliveCostIsTheDifference(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if cost := h.c.keepAliveCost("agt_a", 0.5); cost != 0 {
		t.Fatalf("no earlier figure: %v", cost)
	}
	h.c.keepAliveNoteCost("agt_a", 1.0)
	if cost := h.c.keepAliveCost("agt_a", 1.045); cost < 0.0449 || cost > 0.0451 {
		t.Fatalf("difference = %v", cost)
	}
	if cost := h.c.keepAliveCost("agt_a", 1.09); cost < 0.0449 || cost > 0.0451 {
		t.Fatalf("the new total is the next baseline: %v", cost)
	}
	if cost := h.c.keepAliveCost("agt_a", 0); cost != 0 {
		t.Fatalf("unknown: %v", cost)
	}
	if cost := h.c.keepAliveCost("agt_nobody", 5); cost != 0 {
		t.Fatalf("an agent that is not kept: %v", cost)
	}
}

// How a ping makes way for a delivery depends on where it runs. On this machine it is killed; in a container whose launcher confirms its end it is
// told to terminate (never killed: a kill of the wrapper confirms nothing about the container), and the end counts only when the wrapper exits 143.
func TestKeepAliveYieldsByWhereItRuns(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		container                 bool
		wantTerminate, wantCancel bool
	}{
		{"native", false, false, true},
		{"container that confirms", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newKAHarness(t)
			h.cred.containerOK = true
			h.spawn("agt_a")
			h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = tc.container })
			block := make(chan struct{})
			h.f.mu.Lock()
			h.f.coldBlock = block
			h.f.mu.Unlock()
			h.lastTimer().f()
			eventually(t, "the ping running", func() bool {
				h.c.kaMu.Lock()
				defer h.c.kaMu.Unlock()
				e := h.c.ka["agt_a"]
				return e != nil && e.cancel != nil && func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return h.f.cold == 2 }()
			})
			started := time.Now()
			if !h.c.keepAliveYield("agt_a") {
				t.Fatal("a ping that was ended and confirmed must say so")
			}
			if took := time.Since(started); took > 4*time.Second {
				t.Fatalf("a ping that can be ended delayed the delivery: %v", took)
			}
			eventually(t, "the ping to settle", func() bool { h.c.kaMu.Lock(); defer h.c.kaMu.Unlock(); return h.c.ka["agt_a"].cancel == nil })
			h.f.mu.Lock()
			hold := h.f.lastHold
			h.f.mu.Unlock()
			if hold.terminated.Load() != tc.wantTerminate || hold.cancelled.Load() != tc.wantCancel {
				t.Fatalf("terminated=%v cancelled=%v, want %v %v", hold.terminated.Load(), hold.cancelled.Load(), tc.wantTerminate, tc.wantCancel)
			}
			h.mu.Lock()
			scopes := append([]string(nil), h.scopes...)
			h.mu.Unlock()
			if len(scopes) != 2 || scopes[1] != "off agt_a" {
				t.Fatalf("a confirmed end lifts the scope: %v", scopes)
			}
		})
	}
}

// An agent in a container whose launcher cannot confirm the end of its process is not pinged: a ping could never be known to have ended (a kill of the
// wrapper leaves the process in the container), so none is run and no timer is armed.
func TestKeepAliveNotForAContainerThatCannotConfirmItsEnd(t *testing.T) {
	h := newKAHarness(t)
	h.cred.containerOK = false
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	h.c.kaMu.Lock()
	_, kept := h.c.ka["agt_a"]
	h.c.kaMu.Unlock()
	if kept {
		t.Fatal("an agent whose ping could not be confirmed ended is being kept warm")
	}
	for _, timer := range h.timerList() {
		if timer.d == 3000*time.Second {
			t.Fatal("a ping timer was armed")
		}
	}
}

// quarantined runs a ping in a container whose wrapper is killed after the terminate (so the process in the container is not confirmed gone) and
// returns once it is running and has been told to end. The quarantine and the delivery's wait are shortened.
func (h *kaHarness) quarantined(t *testing.T, quarantine, wait time.Duration) {
	t.Helper()
	h.cred.containerOK = true
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	block := make(chan struct{})
	h.f.mu.Lock()
	h.f.coldBlock = block
	h.f.mu.Unlock()
	h.c.kaQuarantine, h.c.kaWaitTerminate = quarantine, wait // fields of this client, set before any goroutine of it runs
	h.lastTimer().f()
	eventually(t, "the ping running", func() bool {
		h.c.kaMu.Lock()
		defer h.c.kaMu.Unlock()
		e := h.c.ka["agt_a"]
		return e != nil && e.cancel != nil && func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return h.f.cold == 2 }()
	})
	h.f.mu.Lock()
	h.f.lastHold.terminateFails.Store(true) // the launcher cannot confirm: the wrapper is killed after the grace
	h.f.mu.Unlock()
}

// A ping in a container whose wrapper was killed (the terminate was not confirmed) is not over: the toolbox stays refused (the scope is not lifted), the
// agent stays blocked (a real turn is refused, a stop is not acknowledged, a delivery is told to wait) and nothing counts it as ended until the
// quarantine is over; then the scope is lifted and the agent works again.
func TestKeepAliveUnconfirmedContainerEndKeepsTheBlockAndTheScope(t *testing.T) {
	h := newKAHarness(t)
	h.quarantined(t, 1500*time.Millisecond, 100*time.Millisecond)
	if h.c.keepAliveYield("agt_a") {
		t.Fatal("a killed wrapper is not a confirmed end")
	}
	h.mu.Lock()
	scopes := append([]string(nil), h.scopes...)
	h.mu.Unlock()
	for _, word := range scopes {
		if word == "off agt_a" {
			t.Fatalf("the scope must stay on while the process in the container may still run: %v", scopes)
		}
	}
	if !h.c.keepAliveBusy("agt_a") {
		t.Fatal("a delivery must be told to wait")
	}
	spec := adapter.TurnSpec{TurnID: "turn_next", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer waitCancel()
	if _, err := h.c.startTurn(waitCtx, spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("a real turn must not start on the session of a ping that may still run: %v", err)
	}
	if h.c.stopWarm("agt_a", "the agent was stopped") {
		t.Fatal("a stop must not be acknowledged while the ping may still run")
	}
	if h.c.RetireWarmTool("claude", "an account switch") {
		t.Fatal("an account switch must not go on under it")
	}
	if h.f.cold != 2 {
		t.Fatalf("a second process was started: cold=%d", h.f.cold)
	}
	eventually(t, "the quarantine to be over and the scope lifted", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.scopes) >= 2 && h.scopes[len(h.scopes)-1] == "off agt_a"
	})
	eventually(t, "the agent to be free", func() bool { return h.c.keepAliveYield("agt_a") })
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); err != nil {
		t.Fatalf("after the quarantine the turn starts: %v", err)
	}
	if got := h.c.WarmState().UnconfirmedPingEnds; got != 1 {
		t.Fatalf("a quarantine that ran out is counted for Health: %d", got)
	}
}

// A ping that ends unconfirmed stays scoped for the whole quarantine: the supervisor renews the scope (the server's lease is shorter than the ping's run
// and the quarantine together), and only the end of the quarantine lifts it, once.
func TestKeepAliveQuarantineRenewsTheScopeUntilItEnds(t *testing.T) {
	h := newKAHarness(t)
	h.c.kaRenewEvery = 250 * time.Millisecond
	h.quarantined(t, 1500*time.Millisecond, 100*time.Millisecond)
	if h.c.keepAliveYield("agt_a") { // a delivery ends the ping; the wrapper is killed, so the end is not confirmed
		t.Fatal("a killed wrapper is not a confirmed end")
	}
	eventually(t, "the scope to be lifted at the end of the quarantine", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.scopes[len(h.scopes)-1] == "off agt_a"
	})
	h.mu.Lock()
	scopes := append([]string(nil), h.scopes...)
	h.mu.Unlock()
	ons := 0
	for i, word := range scopes {
		if word == "on agt_a" {
			ons++
		} else if i != len(scopes)-1 {
			t.Fatalf("the scope was lifted before the quarantine ended: %v", scopes)
		}
	}
	if ons < 4 {
		t.Fatalf("the scope must be renewed through the quarantine (once at its start, then every period): %v", scopes)
	}
}

// A delivery for an agent in quarantine waits for it to be over, in its own goroutine, and then runs: it does not fail its start after the terminate's
// wait.
func TestKeepAliveDeliveryWaitsForTheQuarantineAndThenRuns(t *testing.T) {
	h := newKAHarness(t)
	h.quarantined(t, 1500*time.Millisecond, 100*time.Millisecond)
	if h.c.keepAliveYield("agt_a") {
		t.Fatal("a killed wrapper is not a confirmed end")
	}
	spec := adapter.TurnSpec{TurnID: "turn_next", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal}
	started := time.Now()
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); err != nil {
		t.Fatalf("the turn must wait for the quarantine and then start: %v", err)
	}
	if took := time.Since(started); took < 500*time.Millisecond {
		t.Fatalf("the turn started inside the quarantine: %v", took)
	}
	h.mu.Lock()
	last := h.scopes[len(h.scopes)-1]
	h.mu.Unlock()
	if last != "off agt_a" {
		t.Fatalf("the scope must be lifted before the turn starts: %v", last)
	}
}

// A containerized ping that exits non-zero on its own (an API error: no signal of ours) has ended inside the container, because `docker exec` passes the
// inner status on: it is confirmed, so there is no quarantine and the scope is lifted at once.
func TestKeepAliveContainerPingFailingOnItsOwnIsConfirmedEnded(t *testing.T) {
	h := newKAHarness(t)
	h.cred.containerOK = true
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	h.f.mu.Lock()
	h.f.coldFail = true
	h.f.mu.Unlock()
	h.fire(t)
	eventually(t, "the scope to be lifted at once", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.scopes) == 2 && h.scopes[1] == "off agt_a"
	})
	if !h.c.keepAliveYield("agt_a") {
		t.Fatal("a ping that failed on its own is gone")
	}
	if h.c.WarmState().UnconfirmedPingEnds != 0 {
		t.Fatal("nothing was left unconfirmed")
	}
}

// Scope frames name their ping, and an owed lift of an earlier ping is settled before the next ping's scope begins, with the earlier ping's name, so the
// server can ignore it if it arrives late.
func TestKeepAliveOwedLiftIsSettledBeforeTheNextPingAndNamesItsPing(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.c.keepAliveOweLift("agt_a", "ka_old")
	h.fire(t)
	eventually(t, "the ping to lift its scope", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.scopes) == 3
	})
	h.mu.Lock()
	scopes, gens := append([]string(nil), h.scopes...), append([]string(nil), h.gens...)
	h.mu.Unlock()
	if scopes[0] != "off agt_a" || gens[0] != "ka_old" || scopes[1] != "on agt_a" || scopes[2] != "off agt_a" {
		t.Fatalf("the owed lift comes first, then the ping's own scope: %v %v", scopes, gens)
	}
	if gens[1] == "" || gens[1] == "ka_old" || gens[1] != gens[2] {
		t.Fatalf("the ping's begin and lift must name the same, new ping: %v", gens)
	}
}

// The numbers cannot drift apart: a ping's own run (its time limit, its terminate and the wait for it) is well inside one scope TTL, and the renewal
// period leaves room for two lost renewals.
func TestKeepAliveScopeTTLCoversAPingsRunAndRenewals(t *testing.T) {
	run := keepAliveTimeout + keepAliveTerminateGrace + keepAliveWaitTerminate
	if run >= protocol.KeepAliveScopeTTL-protocol.KeepAliveScopeRenewEvery {
		t.Fatalf("a ping may run %v before its quarantine begins and renews the scope; the TTL is %v", run, protocol.KeepAliveScopeTTL)
	}
	if 2*protocol.KeepAliveScopeRenewEvery >= protocol.KeepAliveScopeTTL {
		t.Fatal("two lost renewals must not lapse the scope")
	}
}

// A warm_state report counts the pings that run (they are processes on the login though they hold no warm slot), a ping held in quarantine until its
// end is confirmed or the quarantine is over, and none after; each change of that is reported (the server needs it if the connection drops).
func TestWarmReportCountsRunningAndQuarantinedPings(t *testing.T) {
	h := newKAHarness(t)
	if got := h.c.WarmReport().PingsActive; got != 0 {
		t.Fatalf("no ping yet: %d", got)
	}
	h.quarantined(t, 1200*time.Millisecond, 100*time.Millisecond)
	if got := h.c.WarmReport().PingsActive; got != 1 {
		t.Fatalf("a running ping counts: %d", got)
	}
	if h.c.keepAliveYield("agt_a") { // the wrapper is killed: not confirmed, so the ping is quarantined
		t.Fatal("a killed wrapper is not a confirmed end")
	}
	if got := h.c.WarmReport().PingsActive; got != 1 {
		t.Fatalf("a ping held in quarantine still counts: %d", got)
	}
	eventually(t, "the quarantine to be over", func() bool { return h.c.WarmReport().PingsActive == 0 })
	select {
	case <-h.c.warmReports:
	default:
		t.Fatal("the picture's changes must be reported")
	}
}

// A clean stop ends every warm process and ping and then tells the server, before the connection closes, what is left: the last warm_state has no slot
// and no ping, which is what lets the server know the host has nothing running when it is gone.
func TestCleanStopSendsAFinalEmptyWarmState(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck}
	})
	h := newKAHarness(t)
	h.c.ServerURL = peer.server.URL
	h.c.OnError = func(error) {}
	h.c.Warm = WarmConfig{Idle: 5 * time.Minute}
	h.spawn("agt_a")
	h.run("agt_a", nil) // a warm process, idle after its turn
	done := make(chan error, 1)
	go func() { done <- h.c.RunOnce(ctx) }()
	<-peer.hellos
	if got := peer.next(t, protocol.WarmStateFrame); len(got.Warm.Agents) != 1 {
		t.Fatalf("before the stop the idle process is in the report: %+v", got.Warm)
	}
	if err := h.c.StopAcceptingAndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	var last *protocol.WarmReport
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case frame := <-peer.in:
			if frame.Type == protocol.WarmStateFrame {
				last = frame.Warm
			}
			continue
		case <-time.After(300 * time.Millisecond):
			break collect
		case <-deadline:
			break collect
		}
	}
	if last == nil || len(last.Agents) != 0 || last.PingsActive != 0 {
		t.Fatalf("the last report after a clean stop must be empty: %+v", last)
	}
	cancel()
	<-done
}

// A scope lift the server did not answer (the connection dropped) is owed: it is sent again, and its ack waited for, on reconnect and before a real
// turn starts, so that turn's toolbox is never refused for the ping. A turn whose owed lift cannot be confirmed does not start.
func TestKeepAliveOwedScopeLiftIsResentBeforeARealTurnAndOnReconnect(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.mu.Lock()
	h.offErr = errors.New("the connection dropped")
	h.mu.Unlock()
	h.fire(t)
	owed := func() bool { h.c.kaScopeMu.Lock(); defer h.c.kaScopeMu.Unlock(); return h.c.kaOwed["agt_a"] != "" }
	eventually(t, "the lift to be owed", owed)
	eventually(t, "the ping to be over", func() bool { return !h.c.keepAliveBusy("agt_a") })
	spec := adapter.TurnSpec{TurnID: "turn_next", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal}
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); err == nil {
		t.Fatal("a real turn whose toolbox would be refused must not start")
	}
	if !owed() {
		t.Fatal("an unanswered lift stays owed")
	}
	h.mu.Lock()
	h.offErr = nil
	before := len(h.scopes)
	h.mu.Unlock()
	if err := h.c.keepAliveSettleLifts(context.Background(), ""); err != nil || owed() {
		t.Fatalf("a reconnect sends the owed lift again: err=%v owed=%v", err, owed())
	}
	h.mu.Lock()
	scopes := append([]string(nil), h.scopes[before:]...)
	h.mu.Unlock()
	if len(scopes) != 1 || scopes[0] != "off agt_a" {
		t.Fatalf("the owed lift is sent once: %v", scopes)
	}
	// Owed again, answered by the lift that comes before the next real turn.
	h.c.keepAliveOweLift("agt_a", "ka_old")
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); err != nil {
		t.Fatalf("the turn starts once its owed lift is answered: %v", err)
	}
	if owed() {
		t.Fatal("the lift is no longer owed")
	}
}

// A ping is held back for a tool the server says is held (the account is near its limit) and for an agent at its provider limit, and goes ahead when
// the server says the pause is over; the server's frame replaces what it said before.
func TestKeepAliveHeldBackWhilePaused(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.c.applyKeepAlivePaused([]string{"claude"}, nil)
	h.lastTimer().f()
	time.Sleep(60 * time.Millisecond)
	if h.count() != 0 || h.f.cold != 1 {
		t.Fatalf("pinged while the tool was held: reports=%d cold=%d", h.count(), h.f.cold)
	}
	if h.lastTimer().d != keepAliveRetryBusy {
		t.Fatalf("it must look again soon: %v", h.lastTimer().d)
	}
	h.c.applyKeepAlivePaused(nil, []string{"agt_a"})
	h.lastTimer().f()
	time.Sleep(60 * time.Millisecond)
	if h.count() != 0 {
		t.Fatal("pinged an agent at its provider limit")
	}
	h.c.applyKeepAlivePaused(nil, []string{"agt_other"})
	if report := h.fire(t); !report.OK {
		t.Fatalf("once the pause is over it goes ahead: %+v", report)
	}
}

// An account switch ends the pings of its tool: a ping that runs is ended and confirmed before RetireWarmTool answers (the server's check for a running
// turn does not see it), no new one starts while the tool is held, a ping ended this way is not a failure of the ping, and the hold ends with the
// server's next keep_alive_paused frame. A ping whose end is not confirmed refuses the switch.
func TestAccountSwitchEndsRunningPingsAndHoldsNewOnes(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	block := make(chan struct{})
	h.f.mu.Lock()
	h.f.coldBlock = block
	h.f.mu.Unlock()
	h.lastTimer().f() // the ping starts and runs
	eventually(t, "the ping running", func() bool {
		h.c.kaMu.Lock()
		defer h.c.kaMu.Unlock()
		e := h.c.ka["agt_a"]
		return e != nil && e.cancel != nil && func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return h.f.cold == 2 }()
	})
	if !h.c.RetireWarmTool("codex", "an account switch") { // another tool's switch leaves this ping alone
		t.Fatal("retire of another tool failed")
	}
	h.c.kaMu.Lock()
	alive := h.c.ka["agt_a"].cancel != nil
	paused := h.c.kaPause.tools["claude"]
	h.c.kaMu.Unlock()
	if !alive || paused {
		t.Fatalf("a switch of codex touched a claude ping: running=%v held=%v", alive, paused)
	}
	if !h.c.RetireWarmTool("claude", "an account switch") {
		t.Fatal("the switch must go ahead once the ping is confirmed ended")
	}
	h.c.kaMu.Lock()
	entry := h.c.ka["agt_a"]
	ended, held, fails := entry.cancel == nil, h.c.kaPause.tools["claude"], entry.fails
	h.c.kaMu.Unlock()
	if !ended || !held {
		t.Fatalf("the ping must be ended and the tool held when it answers: ended=%v held=%v", ended, held)
	}
	h.f.mu.Lock()
	hold := h.f.lastHold
	h.f.mu.Unlock()
	if !hold.cancelled.Load() {
		t.Fatal("the running ping was not cancelled")
	}
	if fails != 0 {
		t.Fatalf("a ping ended for a switch counted as a failure: %d", fails)
	}
	// Its timer is armed again, finds the hold and looks again soon; nothing starts a ping.
	cold := func() int { h.f.mu.Lock(); defer h.f.mu.Unlock(); return h.f.cold }
	eventually(t, "the timer being armed again", func() bool { h.c.kaMu.Lock(); defer h.c.kaMu.Unlock(); return h.c.ka["agt_a"].timer != nil })
	before := cold()
	h.lastTimer().f()
	time.Sleep(60 * time.Millisecond)
	if cold() != before || h.lastTimer().d != keepAliveRetryBusy {
		t.Fatalf("a ping started, or the timer did not wait, while the tool was held: cold %d->%d timer %v", before, cold(), h.lastTimer().d)
	}
	// The server's frame after the switch replaces the hold, and the pings go on.
	h.c.applyKeepAlivePaused(nil, nil)
	h.f.mu.Lock()
	h.f.coldBlock = nil
	h.f.mu.Unlock()
	if report := h.fire(t); !report.OK {
		t.Fatalf("pings go on after the switch: %+v", report)
	}
}

// A ping is scoped on the server before its process exists and the scope is lifted only after the process is gone, so the server refuses the toolbox for
// exactly as long as the ping can run; both calls are made for every ping.
func TestKeepAlivePingIsScopedAroundItsProcess(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if report := h.fire(t); !report.OK {
		t.Fatalf("report = %+v", report)
	}
	h.mu.Lock()
	scopes, started := append([]string(nil), h.scopes...), append([]int(nil), h.started...)
	h.mu.Unlock()
	if len(scopes) != 2 || scopes[0] != "on agt_a" || scopes[1] != "off agt_a" {
		t.Fatalf("scope calls = %v", scopes)
	}
	if started[0] != 1 || started[1] != 2 {
		t.Fatalf("the scope must be on before the ping's process starts (1 process so far) and lifted after it ran (2): %v", started)
	}
}

// A ping the server did not confirm the scope of does not run: nothing could keep it from acting, so it fails closed, and it counts as a failure.
func TestKeepAliveNotRunWithoutAConfirmedScope(t *testing.T) {
	h := newKAHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.mu.Lock()
	h.scopeErr = errors.New("the server did not confirm the ping's scope")
	h.mu.Unlock()
	report := h.fire(t)
	if report.OK || !strings.Contains(report.Error, "scope") {
		t.Fatalf("report = %+v", report)
	}
	h.f.mu.Lock()
	cold := h.f.cold
	h.f.mu.Unlock()
	if cold != 1 {
		t.Fatalf("a ping whose scope was not confirmed started a process: cold=%d", cold)
	}
	// The server may have applied the scope and its ack been lost: the lift is owed, so the next real turn is not refused for the lease.
	h.c.kaScopeMu.Lock()
	gen := h.c.kaOwed["agt_a"]
	h.c.kaScopeMu.Unlock()
	if gen == "" {
		t.Fatal("an unanswered scope begin must owe a lift")
	}
	h.mu.Lock()
	h.scopeErr = nil
	before := len(h.scopes)
	h.mu.Unlock()
	if err := h.c.keepAliveSettleLifts(context.Background(), "agt_a"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	last, lastGen := h.scopes[len(h.scopes)-1], h.gens[len(h.gens)-1]
	h.mu.Unlock()
	if len(h.scopes) != before+1 || last != "off agt_a" || lastGen != gen {
		t.Fatalf("the owed lift names the ping that could not be scoped: %v %v", last, lastGen)
	}
}

// Without a connection to the server a scope request is refused at once (the real path: no test hook).
func TestKeepAliveScopeCallNeedsTheServer(t *testing.T) {
	h := newKAHarness(t)
	h.c.keepAliveScope = nil
	if err := h.c.keepAliveScopeCall(context.Background(), "agt_a", "ka_x", true); err == nil {
		t.Fatal("a scope request with no server must fail")
	}
	h.c.keepAliveScopeAcked("unknown") // an answer nobody waits for is ignored
}

// A ping in a warm container process that is yielded is ended through the process (Terminate on the warm turn closes it and the launcher confirms), not
// by the turn's own Cancel, which confirms nothing there: without it the agent stayed stuck for ever.
func TestKeepAliveWarmContainerPingIsTerminatedThroughItsProcess(t *testing.T) {
	h := newKAHarness(t)
	h.c.Warm = WarmConfig{Idle: 5 * time.Minute}
	// The ping tells the login again itself (warmCredentials reads the file), so the process is only reused when the real turn's stamp is that file's.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"login":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cred.stamp, h.cred.ok = warmCredentials(adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude"}}, true)
	h.cred.containerOK = true
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true }) // a warm process, idle after its turn
	if !h.idle("agt_a") {
		t.Fatal("no warm process to ping into")
	}
	h.f.mu.Lock()
	h.f.runHold = true
	session := h.f.sessions[0]
	h.f.mu.Unlock()
	h.pingTimer(t).f()
	eventually(t, "the ping running in the warm process", func() bool {
		h.c.kaMu.Lock()
		defer h.c.kaMu.Unlock()
		e := h.c.ka["agt_a"]
		session.mu.Lock()
		defer session.mu.Unlock()
		return e != nil && e.cancel != nil && session.held != nil
	})
	done := make(chan bool, 1)
	go func() { done <- h.c.keepAliveYield("agt_a") }()
	select {
	case ended := <-done:
		if !ended {
			t.Fatal("the ping was not confirmed ended")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a yielded ping in a warm container process never ended")
	}
	if !session.closed {
		t.Fatal("the process must be closed through its launcher (Terminate), not only the turn cancelled")
	}
	if !h.c.keepAliveGone("agt_a") {
		t.Fatal("nothing may be left stuck")
	}
}

// A delivery that has to wait for a ping does not hold up the loop that reads the server's frames: a ping frame is answered while the delivery waits for
// a container ping whose end is not confirmed.
func TestDeliveryWaitingForAPingDoesNotBlockTheFrameLoop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const agentID = "agt_a"
	peer := newMachinePeer(t, activeAck(agentID))
	h := newKAHarness(t)
	c := h.c
	c.ServerURL = peer.server.URL
	c.OnError = func(error) {}
	for i := range c.Harnesses {
		if c.Harnesses[i].Tool == "claude" && c.Harnesses[i].Variant == "native" {
			c.Harnesses[i].Available = true
			c.Harnesses[i].LauncherPath = filepath.Join(t.TempDir(), "claude")
		}
	}
	h.quarantined(t, 4*time.Second, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	<-peer.hellos
	peer.out <- testDelivery(agentID, "turn-wait", "claude", "native", t.TempDir())
	time.Sleep(200 * time.Millisecond) // the delivery is now waiting for the ping
	started := time.Now()
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.Ping}
	peer.next(t, protocol.Pong)
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the frame loop was held up by a delivery waiting for a ping: %v", took)
	}
	cancel()
	<-done
}

// pingTimer is the armed keep-alive timer (not the warm process's idle timer, which may be newer).
func (h *kaHarness) pingTimer(t *testing.T) *fakeTimer {
	t.Helper()
	timers := h.timerList()
	for i := len(timers) - 1; i >= 0; i-- {
		if timers[i].d == time.Duration(h.ka.IntervalSeconds)*time.Second {
			return timers[i]
		}
	}
	t.Fatal("no keep-alive timer is armed")
	return nil
}

// A server's agent_settings frame reaches an agent that is idle: an interval of 0 forgets its keep-alive (a ping that runs is ended), another interval is
// counted from its last real turn at once, a warm window of 0 ends every idle process, and a tool the frame does not name is left alone.
func TestAgentSettingsReachIdleAgents(t *testing.T) {
	h := newKAHarness(t)
	h.c.Warm = WarmConfig{Idle: 5 * time.Minute}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"login":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cred.stamp, h.cred.ok = warmCredentials(adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude"}}, true)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if !h.idle("agt_a") {
		t.Fatal("no idle warm process")
	}
	old := h.pingTimer(t)
	// a different interval is counted from the last real turn: the old timer is stopped and a new one is armed for the new interval
	zero := 300
	h.c.applyAgentSettings(&zero, map[string]protocol.KeepAliveSettings{"codex": {IntervalSeconds: 0}}) // another tool: nothing changes
	h.c.kaMu.Lock()
	_, kept := h.c.ka["agt_a"]
	h.c.kaMu.Unlock()
	if !kept {
		t.Fatal("a setting for another tool forgot the agent")
	}
	h.c.applyAgentSettings(&zero, map[string]protocol.KeepAliveSettings{"claude": {IntervalSeconds: 600, StopAfterSeconds: 86400}})
	h.kaStopped(t, old)
	if got := h.lastTimer(); got.d != 600*time.Second {
		t.Fatalf("the new interval is armed at once: %v", got.d)
	}
	// keep-alive off for the tool: forgotten
	h.c.applyAgentSettings(nil, map[string]protocol.KeepAliveSettings{"claude": {IntervalSeconds: 0}})
	h.c.kaMu.Lock()
	_, kept = h.c.ka["agt_a"]
	h.c.kaMu.Unlock()
	if kept {
		t.Fatal("keep-alive turned off must reach an idle agent")
	}
	// the warm window turned off ends the idle process
	off := 0
	h.c.applyAgentSettings(&off, nil)
	if h.idle("agt_a") {
		t.Fatal("a warm window turned off must end idle processes at once")
	}
}
