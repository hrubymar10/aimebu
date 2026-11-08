package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

var adapterContainerUnconfirmed = errors.New("the process in the container is not confirmed gone")

// warmFake is an adapter whose warm sessions are scripted: no process, only the bookkeeping the supervisor does around one.
type warmFake struct {
	mu       sync.Mutex
	started  int
	cold     int
	sessions []*warmFakeSession
	nextPid  int
	// coldSpecs are the specs of the turns that started a process of their own; coldBlock holds such a start until it is closed (or the turn's
	// context ends); coldFail makes such a turn end without an answer.
	coldSpecs []adapter.TurnSpec
	coldBlock chan struct{}
	lastHold  *holdTurn // the held turn the last blocked start returned
	coldFail  bool
	// onRun runs when a turn is handed to a session.
	onRun func()
	// startErr, when set, is what StartWarm answers; runErr what Run answers.
	startErr, runErr error
	// containerized marks the sessions as running through a Docker launcher (set on the spec by the test).
	// runHold makes a turn handed to a session run until the session is closed or killed, and its own Cancel confirm nothing (as a container's wrapper).
	runHold bool
	// next is what the next turn does: "" ends cleanly with an answer, "fail" ends without one and ends the process.
	next string
}

func (f *warmFake) Catalog() []adapter.CatalogEntry { return nil }
func (f *warmFake) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}
func (f *warmFake) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	f.mu.Lock()
	f.cold++
	f.coldSpecs = append(f.coldSpecs, spec)
	block, fail := f.coldBlock, f.coldFail
	f.mu.Unlock()
	if block != nil {
		hold := newHoldTurn(block)
		f.mu.Lock()
		f.lastHold = hold
		f.mu.Unlock()
		return hold, nil
	}
	return newFakeTurn(!fail), nil
}

// KeepAliveTool says the fake can run keep-alive turns for Claude.
func (f *warmFake) KeepAliveTool() string { return "claude" }
func (f *warmFake) WarmFingerprint(spec adapter.TurnSpec) (string, error) {
	if spec.Mode != adapter.ModeNormal || spec.ForkSession {
		return "", adapter.ErrNoWarm
	}
	return spec.Agent.Model + "|" + spec.Agent.LauncherID, nil
}
func (f *warmFake) StartWarm(_ context.Context, spec adapter.TurnSpec) (adapter.WarmSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started++
	f.nextPid++
	fp, _ := f.WarmFingerprint(spec)
	s := &warmFakeSession{f: f, fp: fp, pid: 1000 + f.nextPid, exited: make(chan struct{}), key: spec.AgentTools}
	f.sessions = append(f.sessions, s)
	return s, nil
}

type warmFakeSession struct {
	f         *warmFake
	fp        string
	pid       int
	exited    chan struct{}
	key       *adapter.AgentToolsAccess
	mu        sync.Mutex
	runs      int
	ended     bool
	killFails atomic.Bool  // Kill and Close report that the process did not end
	failKills atomic.Int32 // the first this many Kill calls fail (Close does too)
	// lingers makes Close and Kill end the host side but report the container side unconfirmed, as a wrapper that was terminated or killed does.
	lingers atomic.Bool
	held    *sessionHeldTurn
	closed  bool // ended by Close (input closed, then it went on its own) rather than Kill
	// onKill runs when the session is killed (the supervisor's state at that moment is what a test looks at).
	onKill func()
}

func (s *warmFakeSession) Fingerprint() string     { return s.fp }
func (s *warmFakeSession) Exited() <-chan struct{} { return s.exited }
func (s *warmFakeSession) Pid() int                { return s.pid }
func (s *warmFakeSession) Alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}
func (s *warmFakeSession) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.ended = true
		close(s.exited)
	}
	if s.held != nil {
		s.held.end(true) // the turn in a process ends with the process
	}
}

// StderrTail is what the fake says its process wrote before it died.
func (s *warmFakeSession) StderrTail() string { return "it fell over" }

// crash ends the process by itself.
func (s *warmFakeSession) crash() { s.end() }

func (s *warmFakeSession) Close(time.Duration) error {
	if s.lingers.Load() {
		s.end()
		return adapterContainerUnconfirmed
	}
	if s.killFails.Load() || s.failKills.Load() > 0 {
		return errors.New("did not end")
	}
	s.closed = true
	if s.onKill != nil {
		s.onKill()
	}
	s.end()
	return nil
}
func (s *warmFakeSession) Kill() error {
	if s.lingers.Load() {
		s.end()
		return adapterContainerUnconfirmed
	}
	if s.killFails.Load() {
		return errors.New("did not end")
	}
	if s.failKills.Load() > 0 {
		s.failKills.Add(-1)
		return errors.New("did not end yet")
	}
	if s.onKill != nil {
		s.onKill()
	}
	s.end()
	return nil
}
func (s *warmFakeSession) Run(adapter.TurnSpec) (adapter.Turn, error) {
	if !s.Alive() {
		return nil, adapter.ErrWarmGone
	}
	if s.f.runErr != nil {
		return nil, s.f.runErr
	}
	s.mu.Lock()
	s.runs++
	s.mu.Unlock()
	if s.f.onRun != nil {
		s.f.onRun()
	}
	s.f.mu.Lock()
	mode := s.f.next
	s.f.next = ""
	s.f.mu.Unlock()
	if mode == "fail" {
		s.end()
		return newFakeTurn(false), nil
	}
	if s.f.runHold {
		turn := &sessionHeldTurn{events: make(chan adapter.Event, 2)}
		s.mu.Lock()
		s.held = turn
		s.mu.Unlock()
		return turn, nil
	}
	return newFakeTurn(true), nil
}

// sessionHeldTurn is a turn in a warm process that ends only when the process does: its Cancel does nothing, as the turn's cancel in a container
// whose wrapper cannot be signalled through `docker exec`.
type sessionHeldTurn struct {
	events chan adapter.Event
	once   sync.Once
	exit   adapter.ExitInfo
}

func (t *sessionHeldTurn) end(cancelled bool) {
	t.once.Do(func() {
		t.exit = adapter.ExitInfo{CancelIntent: cancelled}
		t.events <- adapter.Event{Kind: adapter.EventProcessExit, SourceEventID: "held", Exit: &t.exit}
		close(t.events)
	})
}
func (t *sessionHeldTurn) Events() <-chan adapter.Event { return t.events }
func (t *sessionHeldTurn) Cancel() error                { return nil }
func (t *sessionHeldTurn) Wait() (adapter.ExitInfo, error) {
	<-t.events
	return t.exit, nil
}

type fakeTurn struct {
	events chan adapter.Event
	exit   adapter.ExitInfo
}

// fakeEventSeq makes the usage events of different fake turns different, as a journal needs.
var fakeEventSeq atomic.Int64

func newFakeTurn(clean bool) *fakeTurn {
	t := &fakeTurn{events: make(chan adapter.Event, 6)}
	t.exit = adapter.ExitInfo{TerminalEvent: clean}
	if !clean {
		t.exit.ExitCode = 1
	}
	if clean {
		read, write, in, out := int64(4253), int64(177), int64(2), int64(104)
		t.events <- adapter.Event{Kind: adapter.EventUsage, SourceEventID: fmt.Sprintf("u%d", fakeEventSeq.Add(1)), Usage: &adapter.Usage{TokensIn: &in, TokensOut: &out, TokensCacheRead: &read, TokensCacheWrite: &write}}
		t.events <- adapter.Event{Kind: adapter.EventFinalAnswer, SourceEventID: "a", Answer: json.RawMessage(`{"messages":[]}`)}
	}
	t.events <- adapter.Event{Kind: adapter.EventProcessExit, SourceEventID: "x", Exit: &t.exit}
	close(t.events)
	return t
}
func (t *fakeTurn) Events() <-chan adapter.Event    { return t.events }
func (t *fakeTurn) Cancel() error                   { return nil }
func (t *fakeTurn) Wait() (adapter.ExitInfo, error) { return t.exit, nil }

// warmHarness is a supervisor over a warmFake with a clock and timers a test drives.
type warmHarness struct {
	t      *testing.T
	c      *Client
	f      *warmFake
	now    time.Time
	timers []*fakeTimer
	tmu    sync.Mutex // timers are armed from goroutines too
	turn   int
	cred   credentials
	errs   []error
	ka     *protocol.KeepAliveSettings // the keep-alive setting a delivery carries (nil: none)
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	timer   *time.Timer
	stopped bool
}

func newWarmHarness(t *testing.T) *warmHarness {
	t.Helper()
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	f := &warmFake{}
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, f)
	c.Warm = WarmConfig{Idle: 5 * time.Minute}
	c.ProcessStart = func(pid int) string { return "started-" + strconv.Itoa(pid) }
	h := &warmHarness{t: t, c: c, f: f, now: time.Unix(1_700_000_000, 0), cred: credentials{stamp: "login-1", ok: true, containerOK: true}}
	var errMu sync.Mutex
	c.OnError = func(err error) { errMu.Lock(); h.errs = append(h.errs, err); errMu.Unlock() }
	c.warmNow = func() time.Time { return h.now }
	c.warmAfter = func(d time.Duration, fn func()) *time.Timer {
		ft := &fakeTimer{d: d, f: fn, timer: time.NewTimer(time.Hour)}
		h.tmu.Lock()
		h.timers = append(h.timers, ft)
		h.tmu.Unlock()
		return ft.timer
	}
	c.warmClaim = func(context.Context, uint64, int) error { return nil } // the server's answer to a claim: tests with no connection take it as given
	t.Cleanup(c.shutdownWarm)
	return h
}

func (h *warmHarness) spawn(agent string) {
	key := "aak_" + strings.Repeat(agent[len(agent)-1:], 43)
	if err := h.c.setAgentKey(agent, key); err != nil {
		h.t.Fatal(err)
	}
	h.c.mu.Lock()
	h.c.desired[agent] = protocol.DesiredAgent{AgentID: agent, Generation: 1, Desired: "active", DesiredRev: 1}
	h.c.mu.Unlock()
}

// run runs one turn of an agent the way a delivery does after admission: journal start, start the turn, run it to its end.
func (h *warmHarness) run(agent string, mutate func(*adapter.TurnSpec)) adapter.Turn {
	h.t.Helper()
	h.turn++
	id := "turn_" + string(rune('a'+h.turn))
	ctx := context.Background()
	spec := adapter.TurnSpec{TurnID: id, Agent: adapter.AgentSpec{ID: agent, Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	if mutate != nil {
		mutate(&spec)
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := h.c.Journal.RecordStart(ctx, StartRecord{TurnID: id, AgentID: agent, Generation: 1, DesiredRev: 1, Mode: string(spec.Mode), SpecJSON: specJSON}); err != nil {
		h.t.Fatal(err)
	}
	h.c.mu.Lock()
	h.c.active[agent] = id
	h.c.mu.Unlock()
	h.c.turns.Add(1)
	h.c.keepAliveRemember(spec, h.ka, h.cred, true)
	turn, err := h.c.startTurn(ctx, spec, nil, h.cred)
	if err != nil {
		h.t.Fatal(err)
	}
	h.c.mu.Lock()
	h.c.running[agent] = turn
	h.c.mu.Unlock()
	h.c.runTurn(ctx, protocol.Delivery{AgentID: agent, TurnID: id, Generation: 1, DesiredRev: 1, Mode: spec.Mode, Agent: spec.Agent}, turn)
	return turn
}

func (h *warmHarness) idle(agent string) bool {
	_, ok := h.c.WarmState().Idle[agent]
	return ok
}

func (h *warmHarness) liveTimers() []*fakeTimer {
	var out []*fakeTimer
	for _, ft := range h.timers {
		if !ft.stopped && ft.timer.Stop() { // a timer that could still fire; stopping it here only models "armed"
			ft.timer.Reset(time.Hour)
			out = append(out, ft)
		}
	}
	return out
}

// The second turn of an agent goes into the live process of the first; the process is idle in between, with the window as its timer.
func TestWarmSecondTurnGoesIntoTheLiveProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if h.f.started != 1 || h.f.cold != 0 || !h.idle("agt_a") {
		t.Fatalf("after turn 1: started=%d cold=%d idle=%v", h.f.started, h.f.cold, h.idle("agt_a"))
	}
	if len(h.timers) != 1 || h.timers[0].d != 5*time.Minute {
		t.Fatalf("the window is the timer: %+v", h.timers)
	}
	h.run("agt_a", nil)
	if h.f.started != 1 || h.f.sessions[0].runs != 2 {
		t.Fatalf("the second turn must use the live process: started=%d runs=%d", h.f.started, h.f.sessions[0].runs)
	}
	state := h.c.WarmState()
	if state.Started != 1 || state.Reused != 1 || state.Busy != 0 {
		t.Fatalf("state = %+v", state)
	}
	// The process is out of the admission maps between turns.
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if h.c.active["agt_a"] != "" || h.c.running["agt_a"] != nil {
		t.Fatal("an idle warm process must not count as an active turn")
	}
}

// When the window ends the agent sleeps (its process is closed); the next turn starts a new warm process.
func TestWarmIdleWindowEndsAndTheAgentSleeps(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.timers[0].f() // the window ends
	if h.idle("agt_a") || h.f.sessions[0].Alive() {
		t.Fatal("a sleeping agent has no process")
	}
	h.run("agt_a", nil)
	if h.f.started != 2 || h.f.cold != 0 {
		t.Fatalf("started=%d cold=%d", h.f.started, h.f.cold)
	}
	// A timer that fires after its process was taken for a turn does nothing.
	first := h.timers[1]
	h.run("agt_a", nil)
	first.f()
	if !h.idle("agt_a") || !h.f.sessions[1].Alive() {
		t.Fatal("an old timer ended a process that had been taken and put back")
	}
}

// Anything the process was started with that differs at a delivery ends it and the turn gets a new one.
func TestWarmFingerprintChangeEndsTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.run("agt_a", func(s *adapter.TurnSpec) { s.Agent.Model = "opus" })
	if h.f.started != 2 || h.f.sessions[0].Alive() || !h.f.sessions[1].Alive() {
		t.Fatalf("started=%d first alive=%v", h.f.started, h.f.sessions[0].Alive())
	}
	if h.f.sessions[0].closed {
		t.Fatal("a process being replaced for a delivery is killed, not waited for")
	}
}

// A turn that cannot run warm (a correction, a diagnostic, a fork) ends the live process first, so two processes are never on one session.
func TestWarmColdTurnEndsTheLiveProcessFirst(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].onKill = func() {
		if h.f.cold != 0 {
			t.Error("the cold turn started before the live process ended")
		}
	}
	h.run("agt_a", func(s *adapter.TurnSpec) { s.Mode = adapter.ModeCorrection })
	if h.f.cold != 1 || h.f.sessions[0].Alive() || h.idle("agt_a") {
		t.Fatalf("cold=%d alive=%v idle=%v", h.f.cold, h.f.sessions[0].Alive(), h.idle("agt_a"))
	}
	h.run("agt_a", func(s *adapter.TurnSpec) { s.ForkSession, s.NativeSessionStarted = true, true })
	if h.f.cold != 2 {
		t.Fatalf("a fork runs cold: %d", h.f.cold)
	}
}

// A turn that does not end cleanly takes the process with it; nothing is kept idle.
func TestWarmFailedTurnEndsTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.f.next = "fail"
	h.run("agt_a", nil)
	if h.idle("agt_a") || h.f.sessions[0].Alive() {
		t.Fatal("a failed turn left a process idle")
	}
	h.run("agt_a", nil)
	if h.f.started != 2 {
		t.Fatalf("started = %d", h.f.started)
	}
}

// A stop ends and reaps the idle process before the key is dropped and before the ack: at the moment the process is killed the key is still held.
func TestWarmStopEndsTheProcessBeforeTheKeyGoes(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	keyHeld := false
	h.f.sessions[0].onKill = func() { keyHeld = h.c.keyOf("agt_a") != "" }
	if h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"}) {
		t.Fatal("nothing runs, so the stop ack is not deferred")
	}
	if h.f.sessions[0].Alive() || h.idle("agt_a") {
		t.Fatal("the idle process survived the stop")
	}
	if !h.f.sessions[0].closed {
		t.Fatal("an idle process is closed gracefully first (a Docker wrapper cleans its session up); it is killed only if it does not go")
	}
	if !keyHeld || h.c.keyOf("agt_a") != "" {
		t.Fatalf("order: the process must end while the key is held (held=%v), then the key goes", keyHeld)
	}
}

// A key that is replaced ends the process that holds the old one; the same key again does not.
func TestWarmKeyChangeEndsTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if err := h.c.setAgentKey("agt_a", "aak_"+strings.Repeat("a", 43)); err != nil || !h.f.sessions[0].Alive() {
		t.Fatalf("the same key must leave the process: %v", err)
	}
	if err := h.c.setAgentKey("agt_a", "aak_"+strings.Repeat("z", 43)); err != nil {
		t.Fatal(err)
	}
	if h.f.sessions[0].Alive() {
		t.Fatal("a new key must end the process that holds the old one")
	}
}

// A spawn of the agent again (new configuration) ends its process.
func TestWarmSpawnEndsTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	result := h.c.applySpawn(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, AgentKey: "aak_" + strings.Repeat("a", 43), CheckoutPath: t.TempDir(), Agent: &adapter.AgentSpec{Tool: "fake", Variant: "test"}})
	if result.Result != "ok" {
		t.Fatalf("spawn: %+v", result)
	}
	if h.f.sessions[0].Alive() {
		t.Fatal("a spawn must end the live process")
	}
}

// A process older than its lifetime is not reused and is not kept idle.
func TestWarmLifetime(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm.MaxLifetime = time.Hour
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.now = h.now.Add(61 * time.Minute)
	h.run("agt_a", nil)
	if h.f.started != 2 || h.f.sessions[0].Alive() {
		t.Fatalf("an old process must not take a turn: started=%d", h.f.started)
	}
	// A turn that ends after the lifetime does not leave its process idle.
	h.spawn("agt_b")
	h.f.onRun = func() { h.now = h.now.Add(2 * time.Hour) }
	h.run("agt_b", nil)
	h.f.onRun = nil
	if h.idle("agt_b") {
		t.Fatal("a process past its lifetime was kept idle")
	}
}

// When free memory is low the longest-idle process is ended, one per check; with enough memory nothing is.
func TestWarmMemoryGuardEndsTheLongestIdle(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm.MinFreeBytes = 2 << 30
	free := int64(8 << 30)
	h.c.FreeMemory = func() (int64, int64, error) { return free, 16 << 30, nil }
	for _, agent := range []string{"agt_a", "agt_b", "agt_c"} {
		h.spawn(agent)
		h.run(agent, nil)
		h.now = h.now.Add(time.Second)
	}
	if h.c.guardOnce() {
		t.Fatal("plenty of memory: nothing to end")
	}
	free = 1 << 30
	if !h.c.guardOnce() || h.idle("agt_a") || !h.idle("agt_b") || !h.idle("agt_c") {
		t.Fatalf("the longest-idle (agt_a) must go first: a=%v b=%v c=%v", h.idle("agt_a"), h.idle("agt_b"), h.idle("agt_c"))
	}
	if !h.c.guardOnce() || h.idle("agt_b") || !h.idle("agt_c") {
		t.Fatal("one per check, oldest first")
	}
	free = 8 << 30
	if h.c.guardOnce() || !h.idle("agt_c") {
		t.Fatal("memory recovered: the rest stay")
	}
	free = 1 << 30
	h.c.FreeMemory = func() (int64, int64, error) { return 0, 0, errors.New("unreadable") }
	if h.c.guardOnce() {
		t.Fatal("no reading, no action")
	}
}

// The percentage limit works on its own.
func TestWarmMemoryGuardPercent(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm.MinFreePercent = 10
	h.c.FreeMemory = func() (int64, int64, error) { return 1 << 30, 16 << 30, nil } // 6.25 %
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if !h.c.guardOnce() || h.idle("agt_a") {
		t.Fatal("under 10 % free must end the process")
	}
}

// With warm mode off (the default) every turn runs cold and no warm process exists.
func TestWarmOffRunsEveryTurnCold(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm = WarmConfig{}
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.run("agt_a", nil)
	if h.f.started != 0 || h.f.cold != 2 || len(h.c.WarmState().Idle) != 0 {
		t.Fatalf("started=%d cold=%d", h.f.started, h.f.cold)
	}
}

// Turning warm mode off ends what is idle at the next delivery; RetireAllWarm ends all of it at once.
func TestWarmRetireAll(t *testing.T) {
	h := newWarmHarness(t)
	for _, agent := range []string{"agt_a", "agt_b"} {
		h.spawn(agent)
		h.run(agent, nil)
	}
	h.c.RetireAllWarm("test")
	if h.idle("agt_a") || h.idle("agt_b") || h.f.sessions[0].Alive() || h.f.sessions[1].Alive() {
		t.Fatal("RetireAllWarm left a process")
	}
}

// A stopping supervisor ends its warm processes, and a turn that ends while it stops is not kept.
func TestWarmShutdown(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if err := h.c.StopAcceptingAndDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.idle("agt_a") || h.f.sessions[0].Alive() {
		t.Fatal("the supervisor stopped with a warm process")
	}
}

// A native launcher refused because the instance is docker-only also ends the agent's live process.
func TestWarmDockerOnlyRefusalEndsTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	delivery := protocol.Delivery{AgentID: "agt_a", TurnID: "turn_x", Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{ID: "agt_a", Tool: "codex", Variant: "native"}, DockerOnly: true}
	if err := h.c.handleDeliver(context.Background(), nil, delivery); !errors.Is(err, errDockerOnly) {
		t.Fatalf("err = %v", err)
	}
	if h.f.sessions[0].Alive() {
		t.Fatal("the live process outlived the refusal")
	}
}

// Warm processes are noted in the journal and forgotten when they end; a supervisor that died leaves them for the next one to end, but only
// the process it noted (same start time), never a later one that reused the pid.
func TestWarmOrphansAreEndedAtStart(t *testing.T) {
	h := newWarmHarness(t)
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
		return cmd
	}
	orphan, bystander := start(), start()
	ctx := context.Background()
	if err := h.c.Journal.RecordWarmProcess(ctx, WarmProcess{PID: orphan.Process.Pid, AgentID: "agt_a", Started: processStart(orphan.Process.Pid)}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Journal.RecordWarmProcess(ctx, WarmProcess{PID: bystander.Process.Pid, AgentID: "agt_b", Started: "Mon Jan  1 00:00:00 2001"}); err != nil {
		t.Fatal(err)
	}
	h.c.SweepWarmOrphans(ctx)
	done := make(chan error, 1)
	go func() { done <- orphan.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the orphan was not ended")
	}
	if syscall.Kill(bystander.Process.Pid, 0) != nil {
		t.Fatal("a process with a different start time was ended")
	}
	if left, _ := h.c.Journal.WarmProcesses(ctx); len(left) != 0 {
		t.Fatalf("the journal still notes %v", left)
	}
}

// The journal notes a warm process while it lives and forgets it when it ends.
func TestWarmJournalNotesTheProcess(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	noted, _ := h.c.Journal.WarmProcesses(context.Background())
	if len(noted) != 1 || noted[0].PID != h.f.sessions[0].pid {
		t.Fatalf("noted = %+v", noted)
	}
	h.c.RetireAllWarm("test")
	if noted, _ := h.c.Journal.WarmProcesses(context.Background()); len(noted) != 0 {
		t.Fatalf("an ended process is still noted: %+v", noted)
	}
}

// eventually polls until a condition holds (a watcher goroutine's effect).
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A stop that arrives while a delivery is still handing the turn to the process waits for it: it does not see "nothing to end" and acknowledge
// or drop the key while the process lives. When it returns the process is gone, and it was killed with the key still held.
func TestWarmStopWaitsForADeliveryThatIsStarting(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil) // a process to reuse, so Run is what blocks
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	h.f.onRun = func() { once.Do(func() { close(blocked) }); <-release }
	keyHeld := false
	h.f.sessions[0].onKill = func() { keyHeld = h.c.keyOf("agt_a") != "" }
	ctx := context.Background()
	spec := adapter.TurnSpec{TurnID: "turn_race", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	started := make(chan error, 1)
	go func() { _, err := h.c.startTurn(ctx, spec, nil, h.cred); started <- err }()
	<-blocked
	stopped := make(chan bool, 1)
	go func() {
		stopped <- h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"})
	}()
	select {
	case <-stopped:
		t.Fatal("the stop returned while the process was being handed a turn")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	<-stopped
	if h.f.sessions[0].Alive() {
		t.Fatal("the process outlived the stop that was waiting for it")
	}
	if !keyHeld || h.c.keyOf("agt_a") != "" {
		t.Fatalf("order: killed with the key still held (%v), then the key goes", keyHeld)
	}
}

// A key rotation that arrives in the same window waits too; it leaves the running turn alone but the process does not go idle afterwards.
func TestWarmKeyChangeDuringATurnEndsTheProcessWithIt(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	release := make(chan struct{})
	h.f.onRun = func() { <-release }
	ctx := context.Background()
	spec := adapter.TurnSpec{TurnID: "turn_k", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	turnCh := make(chan adapter.Turn, 1)
	go func() { turn, _ := h.c.startTurn(ctx, spec, nil, h.cred); turnCh <- turn }()
	time.Sleep(100 * time.Millisecond)
	rotated := make(chan struct{})
	go func() { _ = h.c.setAgentKey("agt_a", "aak_"+strings.Repeat("y", 43)); close(rotated) }()
	select {
	case <-rotated:
		t.Fatal("the key change returned while a delivery was starting")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	turn := <-turnCh
	<-rotated
	for range turn.Events() {
	}
	exit, _ := turn.Wait()
	h.c.warmTurnEnded(turn, exit)
	if h.idle("agt_a") || h.f.sessions[0].Alive() {
		t.Fatal("a process whose key changed went idle after its turn")
	}
}

// A stop that lands as a turn ends is settled whichever way they interleave: the process is never left idle after the stop.
func TestWarmStopRacingTheEndOfATurn(t *testing.T) {
	for i := 0; i < 30; i++ {
		h := newWarmHarness(t)
		h.spawn("agt_a")
		release := make(chan struct{})
		h.f.onRun = func() { <-release }
		ctx := context.Background()
		spec := adapter.TurnSpec{TurnID: "turn_s", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
		turnCh := make(chan adapter.Turn, 1)
		go func() { turn, _ := h.c.startTurn(ctx, spec, nil, h.cred); turnCh <- turn }()
		time.Sleep(time.Millisecond)
		close(release)
		turn := <-turnCh
		for range turn.Events() {
		}
		exit, _ := turn.Wait()
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			h.c.mu.Lock()
			h.c.desired["agt_a"] = protocol.DesiredAgent{AgentID: "agt_a", Generation: 1, Desired: "stopped", DesiredRev: 2}
			h.c.mu.Unlock()
			h.c.stopWarm("agt_a", "stop")
		}()
		go func() { defer wait.Done(); h.c.warmTurnEnded(turn, exit) }()
		wait.Wait()
		if h.idle("agt_a") || h.f.sessions[0].Alive() {
			t.Fatalf("round %d: a process was left running after the stop", i)
		}
	}
}

// A process that ends by itself while idle is noticed at once: the agent is asleep, the journal forgets it, and the supervisor's log gets what
// it said, bounded.
func TestWarmIdleCrashIsNoticedAtOnce(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].crash()
	eventually(t, "the idle crash being noticed", func() bool { return !h.idle("agt_a") })
	eventually(t, "the journal forgetting it", func() bool { noted, _ := h.c.Journal.WarmProcesses(context.Background()); return len(noted) == 0 })
	logged := false
	for _, err := range h.errs {
		logged = logged || (strings.Contains(err.Error(), "ended by itself") && strings.Contains(err.Error(), "it fell over"))
	}
	if !logged {
		t.Fatalf("the crash and its stderr were not reported: %v", h.errs)
	}
	h.run("agt_a", nil)
	if h.f.started != 2 {
		t.Fatalf("the next turn must start a new process: %d", h.f.started)
	}
}

// Warm mode never fails a turn: a process that cannot be started or handed the turn is reported and the turn runs cold.
func TestWarmFailuresFallBackToCold(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.f.startErr = errors.New("no memory")
	h.run("agt_a", nil)
	reported := func(text string) bool {
		for _, err := range h.errs {
			if strings.Contains(err.Error(), text) {
				return true
			}
		}
		return false
	}
	if h.f.cold != 1 || !reported("starting a warm process") {
		t.Fatalf("a failed start: cold=%d errs=%v", h.f.cold, h.errs)
	}
	h.f.startErr = nil
	h.f.runErr = errors.New("stdin closed")
	h.run("agt_a", nil)
	if h.f.cold != 2 || h.idle("agt_a") {
		t.Fatalf("a failed hand-over: cold=%d", h.f.cold)
	}
	for _, session := range h.f.sessions {
		if session.Alive() {
			t.Fatal("a process that could not take the turn was left running")
		}
	}
}

// A process that cannot be tracked is not kept warm: with no start time to note, the turn runs cold and the process is ended.
func TestWarmUntrackableProcessRunsCold(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.c.ProcessStart = func(int) string { return "" }
	h.run("agt_a", nil)
	if h.f.cold != 1 || h.idle("agt_a") || h.f.sessions[0].Alive() {
		t.Fatalf("cold=%d idle=%v alive=%v", h.f.cold, h.idle("agt_a"), h.f.sessions[0].Alive())
	}
}

// The journal keeps its note of a process until the process is known to be gone: a kill that did not take leaves the row for the next start.
func TestWarmJournalKeepsTheRowUntilTheProcessIsGone(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].killFails.Store(true)
	h.c.RetireAllWarm("test")
	noted, _ := h.c.Journal.WarmProcesses(context.Background())
	if len(noted) != 1 {
		t.Fatalf("a process that did not end must stay noted: %+v", noted)
	}
}

// A login that cannot be told means no warm process; a changed login means a new one; a Docker launcher that does not share the host's Claude
// home is not told apart by the host's file, so it runs cold.
func TestWarmCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARM_TEST_CLAUDE_DIR", dir)
	spec := adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude"}, LauncherEnvRefs: map[string]string{"CLAUDE_CONFIG_DIR": "WARM_TEST_CLAUDE_DIR"}}
	if _, ok := warmCredentials(spec, true); ok {
		t.Fatal("no credentials file (a login in the keychain, or none) cannot be told")
	}
	file := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	first, ok := warmCredentials(spec, true)
	if !ok || !strings.Contains(first, dir) {
		t.Fatalf("stamp = %q ok=%v: the directory comes from the launcher's own reference", first, ok)
	}
	if err := os.WriteFile(file, []byte(`{"refreshed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	if second, _ := warmCredentials(spec, true); second == first {
		t.Fatal("a refreshed login is another stamp")
	}
	// The same size and the same time with other content (a restored profile) is another stamp too.
	info, _ := os.Stat(file)
	if err := os.WriteFile(file, []byte(`{"refreshed":fals}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	restored, _ := warmCredentials(spec, true)
	if err := os.WriteFile(file, []byte(`{"refreshed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if again, _ := warmCredentials(spec, true); again == restored {
		t.Fatal("same size and time, other content, must be another stamp")
	}
	if _, ok := warmCredentials(spec, false); ok {
		t.Fatal("a launcher that does not share the host's Claude home cannot be told by the host's file")
	}
	if stamp, ok := warmCredentials(adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "codex"}}, false); !ok || stamp != "" {
		t.Fatal("only Claude's login is looked at")
	}
}

// A turn whose login cannot be told runs cold and ends the live process; a different stamp ends it and starts another.
func TestWarmLoginChangeAndUnknownLogin(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.cred = credentials{stamp: "login-2", ok: true, containerOK: true}
	h.run("agt_a", nil)
	if h.f.started != 2 || h.f.sessions[0].Alive() {
		t.Fatalf("a changed login must start a new process: started=%d", h.f.started)
	}
	h.cred = credentials{}
	h.run("agt_a", nil)
	if h.f.cold != 1 || h.f.sessions[1].Alive() || h.idle("agt_a") {
		t.Fatalf("an unknown login runs cold and ends the process: cold=%d", h.f.cold)
	}
}

// For a process in a container the guard also looks at the container side: the Docker host's memory less what the containers use.
func TestWarmMemoryGuardLooksAtDockerForContainerProcesses(t *testing.T) {
	h := newWarmHarness(t)
	h.c.Warm.MinFreeBytes = 2 << 30
	h.c.FreeMemory = func() (int64, int64, error) { return 8 << 30, 16 << 30, nil } // the host is fine
	vmUsed := int64(1 << 30)
	h.c.FreeDockerMemory = func() (int64, int64, error) { return vmUsed, 4 << 30, nil } // the VM: 4 GB, 1 GB used
	h.spawn("agt_n")
	h.run("agt_n", nil)
	if h.c.guardOnce() {
		t.Fatal("no container process, so the VM's memory is not the issue")
	}
	h.now = h.now.Add(time.Second)
	h.spawn("agt_d")
	h.run("agt_d", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	if h.c.guardOnce() {
		t.Fatal("3 GB free on the VM is enough")
	}
	vmUsed = 3 << 30 // 1 GB free of 4
	if !h.c.guardOnce() || h.idle("agt_n") || !h.idle("agt_d") {
		t.Fatal("the VM is short: the longest-idle process (the native one) must go")
	}
	if state := h.c.WarmState(); state.ContainerWideBytes != vmUsed {
		t.Fatalf("container bytes = %d", state.ContainerWideBytes)
	}
}

// A kill that is not confirmed is tried once more, and the second one counts.
func TestWarmEndIsVerifiedASecondTime(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].failKills.Store(1) // Close fails, the first Kill fails, the second works
	if !h.c.RetireAllWarm("test") || h.f.sessions[0].Alive() {
		t.Fatal("the second attempt must end it and confirm it")
	}
	if noted, _ := h.c.Journal.WarmProcesses(context.Background()); len(noted) != 0 {
		t.Fatalf("confirmed gone, so no longer noted: %+v", noted)
	}
}

// A process that cannot be confirmed gone holds everything back: no ack for the stop, the key stays, and no other process is started on the
// session (not even a cold turn). When it is finally gone the stop completes by itself.
func TestWarmUnconfirmedEndWithholdsTheStopAndTheKey(t *testing.T) {
	old := warmRetryEvery
	warmRetryEvery = 10 * time.Millisecond
	defer func() { warmRetryEvery = old }()
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].killFails.Store(true)
	deferred := h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"})
	if !deferred {
		t.Fatal("the stop ack must wait for a process that is not confirmed gone")
	}
	if h.c.keyOf("agt_a") == "" {
		t.Fatal("the key was dropped while a process of the agent may still hold it")
	}
	h.c.mu.Lock()
	_, pending := h.c.pendingStopAck["agt_a"]
	h.c.mu.Unlock()
	if !pending {
		t.Fatal("the ack is not pending")
	}
	time.Sleep(60 * time.Millisecond) // several retries, all failing
	if h.c.keyOf("agt_a") == "" {
		t.Fatal("the key went while the process was still not confirmed gone")
	}
	h.f.sessions[0].killFails.Store(false)
	eventually(t, "the stop completing once the process is gone", func() bool { return h.c.keyOf("agt_a") == "" })
	h.c.mu.Lock()
	_, pending = h.c.pendingStopAck["agt_a"]
	h.c.mu.Unlock()
	if pending || h.f.sessions[0].Alive() {
		t.Fatalf("pending=%v alive=%v", pending, h.f.sessions[0].Alive())
	}
}

// No process, warm or cold, starts on a session while the last one is not confirmed gone; once it is, the next delivery goes ahead.
func TestWarmNoStartAfterAnUnconfirmedKill(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].killFails.Store(true)
	// A turn that runs cold ends the live process first and must not start while it is not confirmed gone.
	spec := adapter.TurnSpec{TurnID: "turn_cold", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeCorrection, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("a cold turn started after an unconfirmed kill: %v", err)
	}
	spec.Mode = adapter.ModeNormal
	spec.Agent.Model = "opus" // a delivery that would replace the process
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("a replacement started after an unconfirmed kill: %v", err)
	}
	if h.f.cold != 0 || h.f.started != 1 {
		t.Fatalf("something started: cold=%d started=%d", h.f.cold, h.f.started)
	}
	h.f.sessions[0].killFails.Store(false)
	h.run("agt_a", func(s *adapter.TurnSpec) { s.Agent.Model = "opus" })
	if h.f.started != 2 {
		t.Fatalf("once the process is gone the next delivery runs: started=%d", h.f.started)
	}
}

// A process in a container is always closed first, on every path that ends one (a changed fingerprint, a failed hand-over, a stop): `docker exec`
// passes no signal on, so a kill of the host side would leave the process in the container running.
func TestWarmContainerProcessesAreAlwaysClosedFirst(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	container := func(s *adapter.TurnSpec) { s.LauncherContainerized = true }
	h.run("agt_a", container)
	h.run("agt_a", func(s *adapter.TurnSpec) { container(s); s.Agent.Model = "opus" }) // a changed fingerprint replaces it
	if !h.f.sessions[0].closed {
		t.Fatal("a containerized process replaced for a delivery must be closed, not killed")
	}
	h.f.runErr = errors.New("stdin closed")
	h.run("agt_a", container) // the hand-over fails: the process ends, and the turn runs cold
	if !h.f.sessions[1].closed {
		t.Fatal("a containerized process that could not take the turn must be closed, not killed")
	}
	h.f.runErr = nil
	h.run("agt_a", container)
	h.c.stopWarm("agt_a", "stop")
	if !h.f.sessions[len(h.f.sessions)-1].closed {
		t.Fatal("a stop closes a containerized process first")
	}
}

// Turning the window off while a process cannot be confirmed gone must not run a second process on the session: the first branch of startTurn
// (warm mode off) is bound by the same rule as the others.
func TestWarmOffWithAnUnconfirmedProcessStartsNothing(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	h.f.sessions[0].killFails.Store(true)
	h.c.mu.Lock()
	h.c.Warm.Idle = 0
	h.c.mu.Unlock()
	spec := adapter.TurnSpec{TurnID: "turn_off", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("a turn started with warm mode off while a process was unconfirmed: %v", err)
	}
	if h.f.cold != 0 {
		t.Fatalf("a cold process started: %d", h.f.cold)
	}
}

// A process in a container is kept warm only when its launcher says it confirms the end of the session inside the container; without that the
// agent runs cold every turn (and nothing is left running).
func TestWarmContainerNeedsALauncherThatConfirmsItsEnd(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.cred.containerOK = false
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	if h.f.started != 0 || h.f.cold != 2 || h.idle("agt_a") {
		t.Fatalf("without the capability: started=%d cold=%d", h.f.started, h.f.cold)
	}
	h.cred.containerOK = true
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	if h.f.started != 1 || !h.idle("agt_a") {
		t.Fatalf("with the capability: started=%d idle=%v", h.f.started, h.idle("agt_a"))
	}
	// The capability going away (a launcher changed) ends the live process before the cold turn.
	h.cred.containerOK = false
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	if h.f.sessions[0].Alive() || h.idle("agt_a") || h.f.cold != 3 {
		t.Fatalf("alive=%v cold=%d", h.f.sessions[0].Alive(), h.f.cold)
	}
}

// A containerized process whose host side ends on a terminate while the container side is not confirmed gone: the slot is stuck, nothing starts on
// the session (not a cold turn, not a replacement), the stop's ack and the key's drop are deferred, and the stop completes only when the end is
// finally confirmed, not before and not because time passed.
func TestWarmContainerSideNeverConfirmedByTime(t *testing.T) {
	old := warmRetryEvery
	warmRetryEvery = 10 * time.Millisecond
	defer func() { warmRetryEvery = old }()
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true })
	h.f.sessions[0].lingers.Store(true)
	if !h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"}) {
		t.Fatal("the stop ack must wait while the container side is unconfirmed")
	}
	if h.f.sessions[0].Alive() {
		t.Fatal("the host side is gone")
	}
	time.Sleep(100 * time.Millisecond) // many retries: time passing confirms nothing
	if h.c.keyOf("agt_a") == "" {
		t.Fatal("the key was dropped while the container side was unconfirmed")
	}
	spec := adapter.TurnSpec{TurnID: "turn_x", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "opus"}, Mode: adapter.ModeCorrection, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	spec.LauncherContainerized = true
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("a turn started on a session whose container side is unconfirmed: %v", err)
	}
	if h.f.cold != 0 || h.f.started != 1 {
		t.Fatalf("something started: cold=%d started=%d", h.f.cold, h.f.started)
	}
	h.f.sessions[0].lingers.Store(false) // the launcher (or an explicit check) now confirms it
	eventually(t, "the stop completing once the end is confirmed", func() bool { return h.c.keyOf("agt_a") == "" })
}

// The instance's window arrives with each delivery: it replaces the supervisor's own, nil leaves it, and 0 ends everything that is idle.
func TestWarmWindowComesWithTheDelivery(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	h.run("agt_a", nil)
	ten := 600
	h.c.applyWarmIdle(&ten)
	if h.c.warmCfg().Idle != 10*time.Minute || !h.idle("agt_a") {
		t.Fatal("a longer window must apply and leave the idle process")
	}
	h.run("agt_a", nil)
	if h.timers[len(h.timers)-1].d != 10*time.Minute {
		t.Fatalf("the next idle window is the new one: %v", h.timers[len(h.timers)-1].d)
	}
	h.c.applyWarmIdle(nil)
	if h.c.warmCfg().Idle != 10*time.Minute {
		t.Fatal("a delivery without the setting must leave the supervisor's own")
	}
	off := 0
	h.c.applyWarmIdle(&off)
	if h.idle("agt_a") || h.f.sessions[0].Alive() || h.c.warmCfg().Idle != 0 {
		t.Fatal("turning warm mode off must end the idle process")
	}
	h.run("agt_a", nil)
	if h.f.started != 1 || h.f.cold != 1 {
		t.Fatalf("with it off a turn runs cold: started=%d cold=%d", h.f.started, h.f.cold)
	}
}

// An account switch retires the idle warm processes of one tool only, and answers false while one cannot be confirmed gone.
func TestWarmRetireByTool(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_claude")
	h.spawn("agt_codex")
	h.run("agt_claude", nil)
	h.run("agt_codex", func(s *adapter.TurnSpec) { s.Agent.Tool = "codex" })
	if !h.c.RetireWarmTool("claude", "an account switch") {
		t.Fatal("confirmed gone, so true")
	}
	if h.idle("agt_claude") || h.f.sessions[0].Alive() {
		t.Fatal("the claude process must end")
	}
	if !h.idle("agt_codex") || !h.f.sessions[1].Alive() {
		t.Fatal("another tool's process must be left alone")
	}
	h.run("agt_claude", nil)
	h.f.sessions[len(h.f.sessions)-1].killFails.Store(true)
	if h.c.RetireWarmTool("claude", "an account switch") {
		t.Fatal("a process that cannot be confirmed gone must answer false, so the switch is refused")
	}
}

// A supervisor with a warm adapter says so in hello; one without does not.
func TestWarmCapabilityIsAdvertisedOnlyForAWarmAdapter(t *testing.T) {
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if !NewClient("m", "k", "http://127.0.0.1:9897", j, &warmFake{}).warmCapable() {
		t.Fatal("a warm adapter must be advertised")
	}
	j2, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if NewClient("m", "k", "http://127.0.0.1:9897", j2, adapter.NewFake()).warmCapable() {
		t.Fatal("an adapter without a warm mode must not be advertised")
	}
}

// The warm report lists agents with a live process (idle until when, or busy), is sent when the picture changes, and is replaced by the next one.
func TestWarmReportAndChangeNotices(t *testing.T) {
	h := newWarmHarness(t)
	h.c.warmReports = make(chan struct{}, 1)
	h.spawn("agt_a")
	h.spawn("agt_b")
	if r := h.c.WarmReport(); len(r.Agents) != 0 {
		t.Fatalf("nothing warm yet: %+v", r)
	}
	h.run("agt_a", nil)
	select {
	case <-h.c.warmReports:
	default:
		t.Fatal("a process going idle must be noticed")
	}
	h.run("agt_b", nil)
	report := h.c.WarmReport()
	if len(report.Agents) != 2 || report.Agents[0].AgentID != "agt_a" || report.Agents[0].State != "idle" || report.Started != 2 {
		t.Fatalf("report = %+v", report)
	}
	want := h.now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	if report.Agents[0].SleepsAt != want {
		t.Fatalf("sleeps_at = %q, want %q", report.Agents[0].SleepsAt, want)
	}
	h.c.RetireAllWarm("test")
	if r := h.c.WarmReport(); len(r.Agents) != 0 {
		t.Fatalf("after retiring: %+v", r)
	}
	// A burst of changes leaves one notice, not one each.
	h.c.warmChanged()
	h.c.warmChanged()
	h.c.warmChanged()
	if len(h.c.warmReports) != 1 {
		t.Fatalf("notices queued: %d", len(h.c.warmReports))
	}
}

// timerList is a copy of the timers armed so far.
func (h *warmHarness) timerList() []*fakeTimer {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	return append([]*fakeTimer(nil), h.timers...)
}

// holdTurn is a turn that runs until it is cancelled or terminated (or released), as a real process does.
type holdTurn struct {
	events     chan adapter.Event
	release    chan struct{}
	once       sync.Once
	exit       adapter.ExitInfo
	cancelled  atomic.Bool
	terminated atomic.Bool
	// terminateFails makes Terminate report that the process did not end on the signal (the wrapper was killed after the grace, as its launcher
	// could not confirm); a confirmed terminate ends the wrapper with 143, a cancel with 137.
	terminateFails atomic.Bool
}

func newHoldTurn(release chan struct{}) *holdTurn {
	t := &holdTurn{events: make(chan adapter.Event, 2), release: release}
	go func() {
		<-release
		t.finish()
	}()
	return t
}

func (t *holdTurn) finish() {
	t.once.Do(func() {
		code := 0
		switch {
		case t.terminated.Load() && !t.terminateFails.Load():
			code = 143
		case t.terminated.Load() || t.cancelled.Load():
			code = 137
		}
		t.exit = adapter.ExitInfo{ExitCode: code, CancelIntent: t.cancelled.Load() || t.terminated.Load()}
		t.events <- adapter.Event{Kind: adapter.EventProcessExit, SourceEventID: "hold", Exit: &t.exit}
		close(t.events)
	})
}

func (t *holdTurn) Events() <-chan adapter.Event { return t.events }
func (t *holdTurn) Cancel() error                { t.cancelled.Store(true); t.finish(); return nil }
func (t *holdTurn) Terminate(time.Duration) bool {
	t.terminated.Store(true)
	t.finish()
	return !t.terminateFails.Load()
}
func (t *holdTurn) Wait() (adapter.ExitInfo, error) {
	<-t.events
	return t.exit, nil
}

// A process is kept warm only once the server has said it has the count that includes it (the claim): the server's knowledge can only be ahead of reality.
// Without that answer (the socket is down, the server is silent or refuses) the process ends with its turn instead and the next turn runs cold.
func TestWarmProcessIsKeptOnlyOnceTheServerHasClaimedIt(t *testing.T) {
	h := newWarmHarness(t)
	var mu sync.Mutex
	var counts []int
	var seqs []uint64
	h.c.warmClaim = func(_ context.Context, seq uint64, count int) error {
		mu.Lock()
		defer mu.Unlock()
		counts, seqs = append(counts, count), append(seqs, seq)
		return nil
	}
	h.spawn("agt_a")
	h.run("agt_a", nil)
	mu.Lock()
	if len(counts) != 1 || counts[0] != 1 {
		t.Fatalf("the claim must count the process about to be kept: %v", counts)
	}
	mu.Unlock()
	if !h.idle("agt_a") || h.c.warmLive.Load() != 1 {
		t.Fatalf("a claimed process is kept: idle=%v live=%d", h.idle("agt_a"), h.c.warmLive.Load())
	}
	h.spawn("agt_b")
	h.c.warmClaim = func(context.Context, uint64, int) error { return errors.New("machine socket disconnected") }
	h.run("agt_b", nil)
	if h.idle("agt_b") {
		t.Fatal("a process the server did not confirm was kept")
	}
	if got := h.c.warmLive.Load(); got != 1 {
		t.Fatalf("the unconfirmed process was ended and no longer counts: live=%d", got)
	}
	if r := h.c.WarmReport(); r.Processes != 1 || len(r.Agents) != 1 || r.Seq == 0 {
		t.Fatalf("report = %+v", r)
	}
}

// Every report and claim takes its number from one counter, so the server can tell which of two is newer.
func TestWarmReportsAndClaimsAreNumberedFromOneCounter(t *testing.T) {
	h := newWarmHarness(t)
	first := h.c.WarmReport().Seq
	second := h.c.WarmReport().Seq
	if second <= first {
		t.Fatalf("report numbers must grow: %d then %d", first, second)
	}
	var claim uint64
	h.c.warmClaim = func(_ context.Context, seq uint64, _ int) error { claim = seq; return nil }
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if claim <= second {
		t.Fatalf("a claim made after a report carries a later number: %d <= %d", claim, second)
	}
	if third := h.c.WarmReport().Seq; third <= claim {
		t.Fatalf("a report after a claim carries a later number: %d <= %d", third, claim)
	}
}

// The real claim path: with no connection to the server it fails at once, so the process is not kept (no hook, as in production).
func TestWarmClaimWithoutAServerConnectionFails(t *testing.T) {
	h := newWarmHarness(t)
	h.c.warmClaim = nil
	h.spawn("agt_a")
	h.run("agt_a", nil)
	if h.idle("agt_a") || h.c.warmLive.Load() != 0 {
		t.Fatalf("a process with no server to claim it was kept: idle=%v live=%d", h.idle("agt_a"), h.c.warmLive.Load())
	}
}

// The slot is not locked while the server's answer to a claim is waited for: a retire of the same agent (what a stop or a spawn on the loop that reads
// the server's frames does) is not held up by it, and what it asked for is honoured when the answer comes: the process is not kept.
func TestWarmClaimDoesNotHoldTheSlotWhileWaiting(t *testing.T) {
	h := newWarmHarness(t)
	asked, release := make(chan struct{}), make(chan struct{})
	h.c.warmClaim = func(ctx context.Context, _ uint64, _ int) error {
		close(asked)
		<-release
		return nil
	}
	h.spawn("agt_a")
	done := make(chan struct{})
	go func() { h.run("agt_a", nil); close(done) }()
	<-asked
	retired := make(chan bool, 1)
	go func() { retired <- h.c.retireWarm("agt_a", "a key change") }()
	select {
	case ok := <-retired:
		if !ok {
			t.Fatal("the retire must succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a retire of the agent was held up by the wait for the server's answer")
	}
	close(release)
	<-done
	if h.idle("agt_a") || h.c.warmLive.Load() != 0 {
		t.Fatalf("a process whose end was asked for while it waited was kept: idle=%v live=%d", h.idle("agt_a"), h.c.warmLive.Load())
	}
}
