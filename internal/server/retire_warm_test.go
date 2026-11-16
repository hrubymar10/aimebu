package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// connectHost makes the host machine look connected with a supervisor that takes retire_warm; the returned function answers the next request.
func connectHost(t *testing.T, f *fixture, supportsWarm bool) *machineConn {
	t.Helper()
	host, err := f.srv.Store.HostMachine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	machine := &machineConn{id: host.ID, supportsWarm: supportsWarm, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.id] = machine
	f.srv.machineMu.Unlock()
	return machine
}

func answerRetire(machine *machineConn, result, message string) {
	request := <-machine.sendQueue
	for request.Type == protocol.KeepAlivePausedFrame { // the hold that goes out first
		request = <-machine.sendQueue
	}
	machine.harnessMu.Lock()
	waiter := machine.pathPending[request.ReqID]
	machine.harnessMu.Unlock()
	if request.Type != protocol.RetireWarm || request.RetireTool != "claude" || waiter == nil {
		close(machine.done)
		return
	}
	waiter <- protocol.Frame{V: protocol.Version, Type: protocol.RetireWarmResult, ReqID: request.ReqID, Result: result, Message: message}
}

// An account switch asks the host's supervisor to end its idle warm processes of the tool and proceeds only when it says they are confirmed gone.
func TestAccountSwitchRetiresWarmAgentsFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	go answerRetire(machine, "ok", "")
	release, err := f.srv.retireHostWarm(context.Background(), "claude")
	if err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	release()
	go answerRetire(machine, "error", "a warm process of the tool is not confirmed gone")
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil || !strings.Contains(err.Error(), "not confirmed gone") {
		t.Fatalf("an unconfirmed end must refuse the switch: %v", err)
	}
}

// A host that is not connected, or whose supervisor does not keep processes warm, is not asked and does not block a switch.
func TestAccountSwitchSkipsAHostWithNothingWarm(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err != nil {
		t.Fatalf("no host connection: %v", err)
	}
	machine := connectHost(t, f, false)
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err != nil {
		t.Fatalf("an old supervisor: %v", err)
	}
	if len(machine.sendQueue) != 0 {
		t.Fatal("a supervisor that does not keep processes warm was sent a frame it cannot handle")
	}
}

// A supervisor that goes away while asked has confirmed nothing, and one that never answers does not either: both refuse the switch.
func TestAccountSwitchWarmRetireGoneOrSilent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	go func() {
		for frame := range machine.sendQueue {
			if frame.Type == protocol.RetireWarm {
				close(machine.done)
				return
			}
		}
	}()
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil {
		t.Fatal("a supervisor that went away before it answered did not confirm anything: the switch is refused")
	}
	silent := connectHost(t, f, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := f.srv.retireHostWarm(ctx, "claude"); err == nil {
		t.Fatal("a supervisor that never answers must refuse the switch")
	}
	_ = silent
}

// A card says warm (until when) while the supervisor reports a live idle process, sleeping when the supervisor says it works to a window and there is
// none, unknown when an older supervisor report lacks idle_seconds, and nothing before the first report
// when the window is 0 (warm mode off), the agent is not active, the machine keeps nothing warm, or the agent is in a turn. Other harnesses say n/a.
func TestAgentCardWarmState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	card := store.AgentCard{ID: "agt_w", MachineID: machine.id, Tool: "claude", Desired: "active", Observed: "idle"}
	if got := f.srv.agentWarmState(card); got != nil {
		t.Fatalf("no report yet: the card stays cold: %+v", got)
	}
	five, zero := 300, 0
	report := func(window *int, agents ...protocol.WarmAgent) {
		machine.warmMu.Lock()
		machine.warm = &protocol.WarmReport{Agents: agents, IdleSeconds: window, Started: 3, Reused: 2, RSSBytes: 5 << 20}
		machine.warmMu.Unlock()
	}
	report(&zero)
	if got := f.srv.agentWarmState(card); got != nil {
		t.Fatalf("window off says nothing (it is not unknown): %+v", got)
	}
	report(nil)
	if got := f.srv.agentWarmState(card); got == nil || got.State != "unknown" {
		t.Fatalf("a report without idle_seconds (an older supervisor) is unknown, never sleeping: %+v", got)
	}
	report(&five)
	if got := f.srv.agentWarmState(card); got == nil || got.State != "sleeping" {
		t.Fatalf("no live process: %+v", got)
	}
	machine.warmMu.Lock()
	machine.warm.Refusals = []protocol.WarmRefusal{{AgentID: card.ID, Reason: "Codex host config key changes turn behavior"}}
	machine.warmMu.Unlock()
	if got := f.srv.agentWarmState(card); got == nil || got.Reason == "" {
		t.Fatalf("cold card lost warm refusal: %+v", got)
	}
	report(&five, protocol.WarmAgent{AgentID: "agt_w", State: "idle", SleepsAt: "2026-11-08T17:00:00Z"}, protocol.WarmAgent{AgentID: "agt_b", State: "busy"})
	if got := f.srv.agentWarmState(card); got == nil || got.State != "warm" || got.SleepsAt != "2026-11-08T17:00:00Z" {
		t.Fatalf("live idle process: %+v", got)
	}
	report(nil, protocol.WarmAgent{AgentID: "agt_w", State: "idle", SleepsAt: "2026-11-08T17:00:00Z"})
	if got := f.srv.agentWarmState(card); got == nil || got.State != "warm" {
		t.Fatalf("a live idle process is warm whatever the report says of its window: %+v", got)
	}
	busy := card
	busy.ID, busy.Observed = "agt_b", "in_turn"
	if got := f.srv.agentWarmState(busy); got != nil {
		t.Fatalf("an agent in a turn says nothing: %+v", got)
	}
	other := card
	other.Tool = "codex"
	if got := f.srv.agentWarmState(other); got == nil || got.State != "na" {
		t.Fatalf("Codex warm mode starts disabled: %+v", got)
	}
	f.srv.codexWarmOn.Store(true)
	if got := f.srv.agentWarmState(other); got == nil || got.State != "warm" {
		t.Fatalf("enabled Codex uses the supervisor's warm report: %+v", got)
	}
	stopped := card
	stopped.Desired = "stopped"
	for name, c := range map[string]store.AgentCard{"stopped": stopped} {
		if got := f.srv.agentWarmState(c); got != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
	old := connectHost(t, f, false)
	_ = old
	if got := f.srv.agentWarmState(card); got != nil {
		t.Fatalf("a machine that keeps nothing warm: %+v", got)
	}
}

func TestWarmRefusalReasonIsGenericForViewer(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_reviewer_default")
	if _, err := f.exec(`UPDATE agent SET tool='claude',observed='idle' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	machine := connectHost(t, f, true)
	five := 300
	machine.warmMu.Lock()
	machine.warm = &protocol.WarmReport{IdleSeconds: &five, Refusals: []protocol.WarmRefusal{{AgentID: agent.ID, Reason: "host config key changes turn behavior"}}}
	machine.warmMu.Unlock()
	viewerCookie, viewerCSRF := f.projectMember(t, "warmviewer", f.project.ID, "viewer")
	read := func(cookie *http.Cookie, csrf string) string {
		resp := f.requestAs(t, cookie, csrf, "GET", "/api/rooms/"+f.room.ID+"/agents", nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("agent cards: %d", resp.StatusCode)
		}
		var cards []struct {
			ID   string     `json:"id"`
			Warm *agentWarm `json:"warm"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
			t.Fatal(err)
		}
		for _, card := range cards {
			if card.ID == agent.ID && card.Warm != nil {
				return card.Warm.Reason
			}
		}
		t.Fatal("agent has no warm card")
		return ""
	}
	if got := read(f.cookie, f.csrf); got != "host config key changes turn behavior" {
		t.Fatalf("owner reason = %q", got)
	}
	if got := read(viewerCookie, viewerCSRF); got != "Warm mode is unavailable for this agent's configuration" {
		t.Fatalf("viewer reason = %q", got)
	}
}

// The machine row carries the supervisor's counts and memory, labelled as not per agent.
func TestMachineRowCarriesTheWarmReport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	host, err := f.srv.Store.HostMachine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row := f.srv.machineRow(host); row.Warm != nil {
		t.Fatalf("no report yet: %+v", row.Warm)
	}
	machine.warmMu.Lock()
	machine.warm = &protocol.WarmReport{Agents: []protocol.WarmAgent{{AgentID: "a", State: "idle"}, {AgentID: "b", State: "idle"}, {AgentID: "c", State: "busy"}}, Started: 5, Reused: 4, RSSBytes: 480 << 20, ContainerWideBytes: 2 << 30}
	machine.warmMu.Unlock()
	row := f.srv.machineRow(host)
	if row.Warm == nil || row.Warm.Idle != 2 || row.Warm.Busy != 1 || row.Warm.Started != 5 || row.Warm.Reused != 4 || row.Warm.RSSBytes != 480<<20 || row.Warm.ContainerWideBytes != 2<<30 {
		t.Fatalf("warm = %+v", row.Warm)
	}
}

// A warm_state frame replaces what the server holds of the machine's warm processes.
func TestWarmStateFrameIsKept(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	card := store.AgentCard{ID: "agt_x", MachineID: machine.id, Tool: "claude", Desired: "active", Observed: "idle"}
	apply := func(report protocol.WarmReport) {
		machine.warmMu.Lock()
		machine.warm = &report
		machine.warmMu.Unlock()
	}
	five := 300
	apply(protocol.WarmReport{Agents: []protocol.WarmAgent{{AgentID: "agt_x", State: "idle", SleepsAt: "2026-11-08T17:00:00Z"}}, IdleSeconds: &five})
	if got := f.srv.agentWarmState(card); got == nil || got.State != "warm" {
		t.Fatalf("warm: %+v", got)
	}
	apply(protocol.WarmReport{Agents: []protocol.WarmAgent{}, IdleSeconds: &five})
	if got := f.srv.agentWarmState(card); got == nil || got.State != "sleeping" {
		t.Fatalf("after the next report: %+v", got)
	}
}

// Keep-alive is on by default (Claude fifty minutes, Codex ten, a day without real work), a delivery carries the interval of its own tool, and
// turning it off or setting a tool to zero sends an interval of zero.
func TestKeepAliveSettingsDefaultsAndDelivery(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	config, err := w.f.srv.keepAliveConfig(t.Context())
	if err != nil || !config.Enabled || config.ClaudeMinutes != 50 || config.CodexMinutes != 10 || config.StopAfterHrs != 24 {
		t.Fatalf("defaults: %+v %v", config, err)
	}
	if got := config.settingsFor("claude"); got.IntervalSeconds != 3000 || got.StopAfterSeconds != 86400 {
		t.Fatalf("claude: %+v", got)
	}
	if got := config.settingsFor("codex"); got.IntervalSeconds != 600 {
		t.Fatalf("codex: %+v", got)
	}
	if got := config.settingsFor("pi"); got.IntervalSeconds != 0 {
		t.Fatalf("a tool that is not kept: %+v", got)
	}
	for _, who := range []adminActor{w.admin, w.pOwner, w.oper, w.viewer, w.out} {
		if code := w.call(t, who, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"enabled": false}}); code != 403 {
			t.Errorf("%s changed keep-alive: %d", who.name, code)
		}
	}
	for _, bad := range []map[string]any{{"claude_minutes": -1}, {"claude_minutes": 56}, {"codex_minutes": 1441}, {"stop_after_hours": 721}} {
		if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": bad}); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"claude_minutes": 30, "codex_minutes": 0, "stop_after_hours": 48}}); code != 200 {
		t.Fatalf("owner sets it: %d", code)
	}
	code, body := w.json(t, w.viewer, "GET", "/api/agent-isolation", nil)
	keep, _ := body["keep_alive"].(map[string]any)
	if code != 200 || keep["enabled"] != true || keep["claude_minutes"] != float64(30) || keep["codex_minutes"] != float64(0) || keep["stop_after_hours"] != float64(48) {
		t.Fatalf("a viewer reads %d %v", code, body)
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"enabled": false}}); code != 200 {
		t.Fatal(code)
	}
	config, _ = w.f.srv.keepAliveConfig(t.Context())
	if config.Enabled || config.settingsFor("claude").IntervalSeconds != 0 || config.ClaudeMinutes != 30 {
		t.Fatalf("off keeps the other values and sends zero: %+v", config)
	}
}

// A card with keep-alive says the interval, how many pings a stretch can have and what they cost at the agent's model and context, and what the
// last ping did; a ping that wrote what it should have read is marked missed. Nothing for an agent it does not apply to.
func TestAgentCardKeepAliveState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	config := keepAliveConfig{Enabled: true, Minutes: map[string]int{"claude": 50, "codex": 4}, ClaudeMinutes: 50, CodexMinutes: 4, StopAfterHrs: 24}
	card := store.AgentCard{ID: "agt_k", MachineID: machine.id, Tool: "claude", Model: "claude-opus-5-5", Desired: "active", Observed: "idle"}
	context := int64(90000)
	usage := &store.AgentUsage{ContextUsed: &context}
	state := f.srv.agentKeepAliveState(card, usage, config)
	if state == nil || state.IntervalMinutes != 50 || state.PingsPerStretch != 28 || state.LastAt != "" {
		t.Fatalf("state = %+v", state)
	}
	machine.warmMu.Lock()
	machine.pings = map[string]protocol.KeepAliveReport{"agt_k": {AgentID: "agt_k", At: "2026-11-08T17:00:00Z", OK: true, CacheRead: 4253, CacheWrite: 177, Cost: 0.045}}
	machine.warmMu.Unlock()
	state = f.srv.agentKeepAliveState(card, usage, config)
	if state.LastOK == nil || !*state.LastOK || state.Missed || state.CacheRead != 4253 || state.CacheWrite != 177 {
		t.Fatalf("a ping that read the cache: %+v", state)
	}
	if state.EstimateUSD == nil || *state.EstimateUSD < 1.25 || *state.EstimateUSD > 1.27 {
		t.Fatalf("the estimate is the last ping's measured cost times 28 pings: %v", state.EstimateUSD)
	}
	machine.warmMu.Lock()
	machine.pings["agt_k"] = protocol.KeepAliveReport{AgentID: "agt_k", At: "2026-11-08T17:50:00Z", OK: true, CacheRead: 0, CacheWrite: 90000}
	machine.warmMu.Unlock()
	if state = f.srv.agentKeepAliveState(card, usage, config); !state.Missed {
		t.Fatalf("a ping that wrote the whole context missed: %+v", state)
	}
	off := config
	off.Enabled = false
	other := card
	other.Tool = "pi"
	stopped := card
	stopped.Desired = "stopped"
	if got := f.srv.agentKeepAliveState(store.AgentCard{ID: "agt_c", MachineID: machine.id, Tool: "codex", Model: "gpt-6-sol", Desired: "active"}, usage, config); got == nil || got.IntervalMinutes != 4 || got.PingsPerStretch != 360 {
		t.Fatalf("a Codex agent is kept too, at its own interval: %+v", got)
	}
	for name, got := range map[string]*agentKeepAlive{"off": f.srv.agentKeepAliveState(card, usage, off), "another tool": f.srv.agentKeepAliveState(other, usage, config), "stopped": f.srv.agentKeepAliveState(stopped, usage, config)} {
		if got != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// What a supervisor is told to hold pings back for: the tools whose limit guard holds turns (only the host is told, the guard judges its account), and
// nothing for a machine with none; a change is a different frame content.
func TestKeepAlivePausedContent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	tools, agents, err := f.srv.keepAlivePaused(context.Background(), machine)
	if err != nil || len(tools) != 0 || len(agents) != 0 {
		t.Fatalf("nothing held: %v %v %v", tools, agents, err)
	}
	f.srv.Store.SetToolHold("claude", "claude session 95%")
	tools, _, err = f.srv.keepAlivePaused(context.Background(), machine)
	if err != nil || len(tools) != 1 || tools[0] != "claude" {
		t.Fatalf("a held tool on the host: %v %v", tools, err)
	}
	other := &machineConn{id: "mch_remote", supportsWarm: true, sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	if tools, _, err = f.srv.keepAlivePaused(context.Background(), other); err != nil || len(tools) != 0 {
		t.Fatalf("a remote machine has its own login: %v %v", tools, err)
	}
	f.srv.Store.SetToolHold("claude", "")
	if tools, _, _ = f.srv.keepAlivePaused(context.Background(), machine); len(tools) != 0 {
		t.Fatalf("released: %v", tools)
	}
}

// An account switch holds the tool's cache pings back from before the supervisor is asked to end its warm processes until the switch is over: the hold
// is in keep_alive_paused (sent first, so the supervisor applies it before it looks for a ping to end), stays while the switch runs, and goes with the
// release. A refused switch releases it at once, and another tool is never held.
func TestAccountSwitchHoldsKeepAlivePings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	next := func() protocol.Frame {
		select {
		case frame := <-machine.sendQueue:
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("no frame")
			return protocol.Frame{}
		}
	}
	go answerRetire(machine, "ok", "")
	release, err := f.srv.retireHostWarm(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	// answerRetire took the hold and the request off the queue. The hold is part of what a machine is told for as long as the switch runs.
	if tools, _, _ := f.srv.keepAlivePaused(context.Background(), machine); len(tools) != 1 || tools[0] != "claude" {
		t.Fatalf("during the switch: %v", tools)
	}
	release()
	frame := next()
	if frame.Type != protocol.KeepAlivePausedFrame || len(frame.PausedTools) != 0 {
		t.Fatalf("the release lifts the hold: %+v", frame)
	}
	if tools, _, _ := f.srv.keepAlivePaused(context.Background(), machine); len(tools) != 0 {
		t.Fatalf("after the switch: %v", tools)
	}
	release() // a second release changes nothing
	if len(machine.sendQueue) != 0 {
		t.Fatalf("a second release sent a frame: %+v", <-machine.sendQueue)
	}
	go answerRetire(machine, "error", "a ping is not confirmed ended")
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil {
		t.Fatal("an unconfirmed end refuses the switch")
	}
	if tools, _, _ := f.srv.keepAlivePaused(context.Background(), machine); len(tools) != 0 {
		t.Fatalf("a refused switch holds nothing: %v", tools)
	}
}

// Claude's interval is capped at 55 minutes (the prompt cache lives an hour, so a later ping always finds it gone): the owner cannot set more, and a
// larger value stored before the cap is read as 55.
func TestKeepAliveClaudeIntervalIsCapped(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"claude_minutes": 55}}); code != 200 {
		t.Fatalf("55 is allowed: %d", code)
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"claude_minutes": 56}}); code != 400 {
		t.Fatalf("56 is refused: %d", code)
	}
	if err := w.f.srv.Store.SetGlobalSettingValue(t.Context(), keepAliveClaudeKey, "90"); err != nil {
		t.Fatal(err)
	}
	config, err := w.f.srv.keepAliveConfig(t.Context())
	if err != nil || config.ClaudeMinutes != 55 || config.settingsFor("claude").IntervalSeconds != 55*60 {
		t.Fatalf("a stored 90 reads as the cap: %+v %v", config, err)
	}
}

// The warm window and the keep-alive settings go to a connected supervisor without a delivery: at once when the owner saves a change (an idle agent
// gets no delivery), and only when they changed (the periodic check sends nothing new).
func TestAgentSettingsReachAnIdleSupervisor(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	machine := connectHost(t, w.f, true)
	settings := func() protocol.Frame {
		t.Helper()
		select {
		case frame := <-machine.sendQueue:
			if frame.Type == protocol.KeepAlivePausedFrame {
				return settingsAfter(t, machine)
			}
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("no agent_settings frame")
			return protocol.Frame{}
		}
	}
	w.f.srv.sendAgentSettings(t.Context(), machine)
	frame := settings()
	if frame.Type != protocol.AgentSettingsFrame || frame.WarmIdleSeconds != nil || frame.KeepAliveByTool["claude"].IntervalSeconds != 3000 || frame.KeepAliveByTool["codex"].IntervalSeconds != 600 {
		t.Fatalf("first frame: %+v", frame)
	}
	w.f.srv.sendAgentSettings(t.Context(), machine)
	if len(machine.sendQueue) != 0 {
		t.Fatal("the same settings were sent again")
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]any{"keep_alive": map[string]any{"enabled": false}, "warm_idle_minutes": 5}); code != 200 {
		t.Fatalf("owner saves: %d", code)
	}
	frame = settings()
	if frame.WarmIdleSeconds == nil || *frame.WarmIdleSeconds != 300 || frame.KeepAliveByTool["claude"].IntervalSeconds != 0 || frame.KeepAliveByTool["codex"].IntervalSeconds != 0 {
		t.Fatalf("a saved change must reach the supervisor at once, keep-alive off as an interval of 0: %+v", frame)
	}
	old := connectHost(t, w.f, false)
	w.f.srv.sendAgentSettings(t.Context(), old)
	if len(old.sendQueue) != 0 {
		t.Fatal("a supervisor that keeps nothing warm was sent a frame it cannot handle")
	}
}

// settingsAfter reads on to the next agent_settings frame.
func settingsAfter(t *testing.T, machine *machineConn) protocol.Frame {
	t.Helper()
	for {
		select {
		case frame := <-machine.sendQueue:
			if frame.Type == protocol.AgentSettingsFrame {
				return frame
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no agent_settings frame")
			return protocol.Frame{}
		}
	}
}

// A ping's cost on the card is the server's difference from the session's last running total, not the supervisor's, which cannot tell it for the first
// ping after it restarted (it reports 0 and the card showed $0).
func TestKeepAliveCardCostComesFromTheServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	agent, _ := f.createToolAgent(t, "role_leader_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET home_room_id=? WHERE id=?`, f.room.ID, agent.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_ka',?,0,'fresh_with_history','2026-11-08T10:00:00.000000000Z')`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,session_id,agent_id,room_id,project_id,tokens_in,tokens_out,cost_est,cost_total,model,partial,source,created_at)
			VALUES ('use_prev','prev',NULL,'ses_ka',?,?,?,1,1,1.0,1.0,'m',0,'measured','2026-11-08T10:01:00.000000000Z')`, agent.ID, f.room.ID, f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	report := protocol.KeepAliveReport{AgentID: agent.ID, At: "2026-11-08T17:00:00Z", OK: true, CacheRead: 90000, CacheWrite: 150, Cost: 0, CostCumulative: 1.045, CostIsTotal: true}
	machine.warmMu.Lock()
	machine.pings = map[string]protocol.KeepAliveReport{agent.ID: report}
	machine.warmMu.Unlock()
	f.srv.recordKeepAliveUsage(t.Context(), machine, report)
	machine.warmMu.Lock()
	got := machine.pings[agent.ID].Cost
	machine.warmMu.Unlock()
	if got < 0.0449 || got > 0.0451 {
		t.Fatalf("the card's ping cost = %v, want the server's 0.045", got)
	}
}

// A host that is not connected is not taken to have no warm processes: a dropped connection or a restart of the server leaves the supervisor's idle
// processes running. The switch is refused unless the host never connected, or its last word was that it keeps nothing warm.
func TestAccountSwitchRefusedWhileTheHostMayStillHaveWarmAgents(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	host, err := f.srv.Store.HostMachine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err != nil {
		t.Fatalf("a host that never connected has nothing warm: %v", err)
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine SET last_seen=? WHERE id=?`, "2026-11-08T10:00:00Z", host.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("a host that connected before and is gone, with nothing known of it, may still have warm agents: %v", err)
	}
	f.srv.noteWarmSeen(host.ID, 2)
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil {
		t.Fatal("a host whose last warm_state listed agents may still have them")
	}
	f.srv.noteWarmSeen(host.ID, 0)
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err != nil {
		t.Fatalf("a host whose last warm_state said none has none: %v", err)
	}
	// A ping that began before the drop is a process on the login too: the scope the server holds for it says so, even when the last report said
	// nothing (it was built before the ping began).
	f.srv.scopeKeepAlive("agt_ping", host.ID, "ka_x", true)
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err == nil {
		t.Fatal("a ping of the host may still run: the switch must be refused")
	}
	f.srv.scopeKeepAlive("agt_ping", host.ID, "ka_x", false)
	if _, err := f.srv.retireHostWarm(context.Background(), "claude"); err != nil {
		t.Fatalf("the ping ended and lifted its scope: %v", err)
	}
}

// The host's last report is kept in the database: a server that restarted (its memory is empty) still knows the host had nothing running, so switching
// is not wedged, and still refuses when the last word was something running. Revoking the machine forgets it.
func TestHostsLastWarmReportSurvivesARestartOfTheServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	host, err := f.srv.Store.HostMachine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine SET last_seen=? WHERE id=?`, "2026-11-08T10:00:00Z", host.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	restart := func() { f.srv.warmSeenMu.Lock(); f.srv.warmSeen = nil; f.srv.warmSeenMu.Unlock() }
	f.srv.noteWarmSeen(host.ID, 0)
	restart()
	if _, err := f.srv.retireHostWarm(ctx, "claude"); err != nil {
		t.Fatalf("after a restart the persisted 0 allows the switch: %v", err)
	}
	f.srv.noteWarmSeen(host.ID, 2)
	restart()
	if _, err := f.srv.retireHostWarm(ctx, "claude"); err == nil {
		t.Fatal("after a restart a persisted count above 0 refuses the switch")
	}
	// A ping about to run raises it even over a 0.
	f.srv.noteWarmSeen(host.ID, 0)
	machine := connectHost(t, f, true)
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "p", AgentID: "agt_ping", ScopeOn: true, ScopeGen: "ka_p"}, func(string) bool { return true })
	<-machine.sendQueue
	f.srv.machineMu.Lock()
	delete(f.srv.machines, machine.id)
	f.srv.machineMu.Unlock()
	f.srv.scopeKeepAlive("agt_ping", host.ID, "ka_p", false) // lifted: only the persisted mark is left
	restart()
	if _, err := f.srv.retireHostWarm(ctx, "claude"); err == nil {
		t.Fatal("a ping that was about to run when the report was last updated must refuse the switch")
	}
	f.srv.forgetWarmSeen(ctx, host.ID)
	if _, ok, _ := f.srv.Store.GlobalSettingValue(ctx, warmSeenKeyPrefix+host.ID); ok {
		t.Fatal("the saved report must go with the machine")
	}
}

// Counts the supervisor gives are applied in the order of their numbers whatever order the frames are handled in: an older number never overwrites a
// newer one, in memory or in the saved record, even when two frames race (the 1 then 0 of a process kept and then ended).
func TestWarmCountsApplyInTheOrderOfTheirNumbers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	machine := connectHost(t, f, true)
	saved := func() int {
		raw, ok, err := f.srv.Store.GlobalSettingValue(ctx, warmSeenKeyPrefix+machine.id)
		if err != nil || !ok {
			t.Fatalf("no saved report: %v %v", ok, err)
		}
		var record warmSeenRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			t.Fatal(err)
		}
		return record.Count
	}
	held := func() int {
		f.srv.warmSeenMu.Lock()
		defer f.srv.warmSeenMu.Unlock()
		return f.srv.warmSeen[machine.id].count
	}
	f.srv.applyWarmCount(machine, 6, 0, false) // the newer one (process ended) is handled first...
	f.srv.applyWarmCount(machine, 5, 1, false) // ...then the older (process kept) arrives late
	if held() != 0 || saved() != 0 {
		t.Fatalf("an older number overwrote a newer: held=%d saved=%d", held(), saved())
	}
	f.srv.applyWarmCount(machine, 7, 2, false)
	if held() != 2 || saved() != 2 {
		t.Fatalf("a newer number is applied: held=%d saved=%d", held(), saved())
	}
	// A ping about to run raises the count with its own number and never lowers it; an older report cannot undo it.
	f.srv.applyWarmCount(machine, 9, 1, true)
	if held() != 2 {
		t.Fatalf("a raise never lowers: %d", held())
	}
	f.srv.applyWarmCount(machine, 11, 0, false)
	f.srv.applyWarmCount(machine, 12, 1, true)
	f.srv.applyWarmCount(machine, 10, 0, false) // built before the ping, handled after it
	if held() != 1 || saved() != 1 {
		t.Fatalf("a report older than the ping's own number undid it: held=%d saved=%d", held(), saved())
	}
	// Racing frames: whatever the interleaving, the highest number's count is what is held and saved.
	var wait sync.WaitGroup
	for seq := uint64(100); seq <= 140; seq++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			f.srv.applyWarmCount(machine, seq, int(seq%2), false)
		}()
	}
	wait.Wait()
	if held() != 0 || saved() != 0 {
		t.Fatalf("racing frames: the last number (140, count 0) must win: held=%d saved=%d", held(), saved())
	}
	// A count from a connection that is no longer the machine's is ignored.
	stale := &machineConn{id: machine.id, sendQueue: make(chan protocol.Frame, 2), done: make(chan struct{})}
	if f.srv.applyWarmCount(stale, 1000, 5, false) || held() != 0 {
		t.Fatal("a stale connection's count was applied")
	}
}

// A warm claim is saved before it is answered, and a stale connection is not answered (its supervisor then ends the process).
func TestWarmClaimIsSavedThenAnswered(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	f.srv.handleWarmClaim(machine, protocol.Frame{ReqID: "c1", WarmSeq: 3, WarmCount: 1})
	ack := <-machine.sendQueue
	if ack.Type != protocol.WarmClaimAckFrame || ack.ReqID != "c1" {
		t.Fatalf("ack = %+v", ack)
	}
	f.srv.warmSeenMu.Lock()
	count := f.srv.warmSeen[machine.id].count
	f.srv.warmSeenMu.Unlock()
	raw, ok, _ := f.srv.Store.GlobalSettingValue(context.Background(), warmSeenKeyPrefix+machine.id)
	if count != 1 || !ok || !strings.Contains(raw, `"count":1`) {
		t.Fatalf("the claim must be held and saved before it is answered: held=%d saved=%q", count, raw)
	}
	stale := &machineConn{id: machine.id, sendQueue: make(chan protocol.Frame, 2), done: make(chan struct{})}
	f.srv.handleWarmClaim(stale, protocol.Frame{ReqID: "c2", WarmSeq: 4, WarmCount: 1})
	if len(stale.sendQueue) != 0 {
		t.Fatal("a claim from a connection that is not the machine's must not be answered")
	}
}

// A count that cannot be saved is not acknowledged (the supervisor then ends the process, or the ping does not run) and memory is not moved ahead of
// what was saved: after a restart and a drop the saved 0 would otherwise let a switch through under a process the server said it had.
func TestWarmCountThatCannotBeSavedIsNotAcknowledged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	f.srv.noteWarmSeen(machine.id, 0)
	f.srv.saveWarmSeen = func(context.Context, string, string) error { return errors.New("disk full") }
	f.srv.handleWarmClaim(machine, protocol.Frame{ReqID: "c1", WarmSeq: 3, WarmCount: 1})
	if len(machine.sendQueue) != 0 {
		t.Fatalf("a claim that was not saved was answered: %+v", <-machine.sendQueue)
	}
	f.srv.warmSeenMu.Lock()
	held := f.srv.warmSeen[machine.id].count
	f.srv.warmSeenMu.Unlock()
	if held != 0 {
		t.Fatalf("memory moved ahead of what was saved: %d", held)
	}
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "s1", AgentID: "agt_ping", ScopeOn: true, ScopeGen: "ka_s", WarmSeq: 4}, func(string) bool { return true })
	if len(machine.sendQueue) != 0 || f.srv.keepAliveScoped("agt_ping") {
		t.Fatal("a ping whose count was not saved must not be scoped and answered, so it does not run")
	}
	f.srv.saveWarmSeen = nil
	f.srv.handleWarmClaim(machine, protocol.Frame{ReqID: "c2", WarmSeq: 5, WarmCount: 1})
	if ack := <-machine.sendQueue; ack.ReqID != "c2" {
		t.Fatalf("once it can be saved the claim is answered: %+v", ack)
	}
}

// A warm process's real end is recorded as an event naming the agent and the reason, but only when it comes from the machine that runs the agent; the
// reason is bounded.
func TestWarmProcessEndedFrameRecordsAnEventFromTheAgentsMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	agent, _ := f.createToolAgent(t, "role_worker_default")
	runtime, err := f.db.AgentRuntimeByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	machine.id = runtime.MachineID
	events := func() (count int, longest int) {
		t.Helper()
		all, err := f.db.EventsAfter(t.Context(), f.room.CreatedBy, 0, 1<<62)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range all {
			if e.Kind == "warm_process_ended" && e.AgentID == agent.ID {
				count++
				longest = max(longest, len(e.PayloadJSON))
			}
		}
		return count, longest
	}
	f.srv.handleWarmEnded(t.Context(), machine, protocol.Frame{Type: protocol.WarmEndedFrame, AgentID: agent.ID, Message: strings.Repeat("x", 500)})
	if n, longest := events(); n != 1 || longest > 260 {
		t.Fatalf("the end of a warm process must be recorded once, its reason bounded: %d events, %d bytes", n, longest)
	}
	other := &machineConn{id: "mch_other", sendQueue: make(chan protocol.Frame, 2), done: make(chan struct{})}
	f.srv.handleWarmEnded(t.Context(), other, protocol.Frame{Type: protocol.WarmEndedFrame, AgentID: agent.ID, Message: "not mine"})
	f.srv.handleWarmEnded(t.Context(), machine, protocol.Frame{Type: protocol.WarmEndedFrame, AgentID: "agt_nobody", Message: "no such agent"})
	if n, _ := events(); n != 1 {
		t.Fatalf("a machine that does not run the agent, or an unknown agent, must not add one: %d", n)
	}
}
