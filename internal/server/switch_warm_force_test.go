package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// shortRetireWait makes the server's bounded wait for unconfirmed warm processes short.
func shortRetireWait(f *fixture) {
	f.srv.retireWait, f.srv.retireEvery = 300*time.Millisecond, 50*time.Millisecond
}

// answerRetireBlocked answers every retire_warm request on the machine with "error" naming blocked, until stop is closed; asks counts them.
func answerRetireBlocked(machine *machineConn, blocked []string, asks *atomic.Int32, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case request := <-machine.sendQueue:
			if request.Type != protocol.RetireWarm {
				continue
			}
			asks.Add(1)
			machine.harnessMu.Lock()
			waiter := machine.pathPending[request.ReqID]
			machine.harnessMu.Unlock()
			if waiter != nil {
				waiter <- protocol.Frame{V: protocol.Version, Type: protocol.RetireWarmResult, ReqID: request.ReqID, Result: "error",
					Message: "a warm process of the tool is not confirmed gone", RetireBlocked: blocked}
			}
		}
	}
}

// One warm process the host's supervisor cannot confirm ended: the switch asks again for a bounded time, then refuses with 409 warm_agents naming
// the agent and offering force; sent again with force_warm it switches, and the agent is quarantined (no new turn, no ping) until its supervisor
// reports the process ended.
func TestAWebSwitchWaitsForAnUnconfirmedWarmAgentThenNamesItAndForceQuarantinesIt(t *testing.T) {
	f, m, _ := quotaEnv(t)
	f.seed()
	shortRetireWait(f)
	agentID := hostAgent(t, f, "warm-stuck", "claude")
	machine := connectHost(t, f, true)
	var asks atomic.Int32
	stop := make(chan struct{})
	defer close(stop)
	go answerRetireBlocked(machine, []string{agentID}, &asks, stop)

	started := time.Now()
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	var refusal struct {
		Error    string             `json:"error"`
		Message  string             `json:"message"`
		Agents   []store.AgentLabel `json:"agents"`
		CanForce bool               `json:"can_force"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&refusal)
	resp.Body.Close()
	if resp.StatusCode != 409 || refusal.Error != "warm_agents" || !refusal.CanForce {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, refusal)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond || asks.Load() < 3 {
		t.Fatalf("the switch did not wait and ask again: %v, %d asks", waited, asks.Load())
	}
	labels, err := f.srv.Store.AgentLabels(context.Background(), []string{agentID})
	if err != nil || len(labels) != 1 || !strings.HasSuffix(labels[0].Label, "@p") {
		t.Fatalf("labels = %+v %v", labels, err)
	}
	label := labels[0].Label
	if len(refusal.Agents) != 1 || refusal.Agents[0].ID != agentID || refusal.Agents[0].Label != label ||
		refusal.Message != "these agents kept warm could not be confirmed ended: "+label {
		t.Fatalf("the refusal does not name the agent: %+v", refusal)
	}
	if activeProfile(t, m) != "A" || f.srv.Store.AgentQuarantined(agentID) || f.srv.Store.DispatchHeldForSwitch("claude") || f.srv.kaSwitchingTool("claude") {
		t.Fatal("a refused switch must change nothing and release its holds")
	}

	resp = f.request(http.MethodPost, "/api/switcher/claude/use", map[string]any{"name": "B", "force_warm": true}, true)
	var switched struct {
		Active      string             `json:"active"`
		Switched    bool               `json:"switched"`
		Quarantined []store.AgentLabel `json:"quarantined"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&switched)
	resp.Body.Close()
	if resp.StatusCode != 200 || !switched.Switched || switched.Active != "B" || activeProfile(t, m) != "B" {
		t.Fatalf("forced switch: status=%d %+v active=%s", resp.StatusCode, switched, activeProfile(t, m))
	}
	if len(switched.Quarantined) != 1 || switched.Quarantined[0].Label != label {
		t.Fatalf("quarantined = %+v", switched.Quarantined)
	}
	if !f.srv.Store.AgentQuarantined(agentID) {
		t.Fatal("the agent the switch went past is not quarantined")
	}
	ctx := context.Background()
	if _, agents, err := f.srv.keepAlivePaused(ctx, machine); err != nil || !contains(agents, agentID) {
		t.Fatalf("its pings are not held: %v %v", agents, err)
	}
	var recorded bool
	for _, event := range systemEvents(t, f) {
		recorded = recorded || event.Kind == "agent_switch_quarantined"
	}
	if !recorded {
		t.Fatal("the quarantine is not on the record")
	}

	// The supervisor reports the process ended: the quarantine lifts.
	f.srv.handleWarmEnded(ctx, machine, protocol.Frame{Type: protocol.WarmEndedFrame, AgentID: agentID, Message: "its container is gone"})
	if f.srv.Store.AgentQuarantined(agentID) {
		t.Fatal("a confirmed end must lift the quarantine")
	}
	f.srv.WaitUsageRefreshes()
}

// A quarantine ends by itself when its time is up.
func TestASwitchQuarantineHoldsDispatchAndExpires(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.Store.QuarantineAgent("agt_x", time.Now().Add(50*time.Millisecond))
	if !f.srv.Store.AgentQuarantined("agt_x") || !contains(f.srv.Store.QuarantinedAgents(), "agt_x") {
		t.Fatal("not quarantined")
	}
	time.Sleep(80 * time.Millisecond)
	if f.srv.Store.AgentQuarantined("agt_x") || len(f.srv.Store.QuarantinedAgents()) != 0 {
		t.Fatal("an expired quarantine still holds")
	}
	if f.srv.Store.ReleaseQuarantine("agt_x") {
		t.Fatal("an expired quarantine is not one in force")
	}
}

// Force never goes past a supervisor that did not answer: nothing is known about what it runs.
func TestForceDoesNotGoPastASilentSupervisor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	connectHost(t, f, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := f.srv.retireHostWarmForce(ctx, "claude", true); err == nil {
		t.Fatal("a supervisor that never answers must refuse even a forced switch")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
