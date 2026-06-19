package server

import (
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestSweepAgentLivenessLadder(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "ladder")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	for _, tc := range []struct {
		name     string
		age      time.Duration
		want     string
		wantEvts int
	}{
		{name: "fresh stays unset", age: time.Minute, want: "", wantEvts: 0},
		{name: "stale badge", age: 2 * time.Minute, want: types.AgentStateStale, wantEvts: 0},
		{name: "offline alert", age: 11 * time.Minute, want: types.AgentStateOffline, wantEvts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.mu.Lock()
			s.agents[agent.ID].State = ""
			s.agents[agent.ID].StateAt = time.Time{}
			s.agents[agent.ID].LastSeen = now.Add(-tc.age).Format(time.RFC3339)
			s.mu.Unlock()

			events := s.sweepAgentLiveness(now)
			got := findListedAgent(t, s, agent.ID)
			if got.State != tc.want {
				t.Fatalf("state = %q, want %q", got.State, tc.want)
			}
			if len(events) != tc.wantEvts {
				t.Fatalf("events = %d, want %d", len(events), tc.wantEvts)
			}
		})
	}
}

func TestSweepAgentLivenessOfflineEdgeFiresOnceAndReconnectRearms(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "edge")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	setAgentLastSeen(t, s, agent.ID, now.Add(-11*time.Minute))

	events := s.sweepAgentLiveness(now)
	if len(events) != 1 || events[0].Kind != livenessEventOffline {
		t.Fatalf("first sweep events = %#v, want one offline", events)
	}
	events = s.sweepAgentLiveness(now.Add(15 * time.Second))
	if len(events) != 0 {
		t.Fatalf("second sweep events = %#v, want none", events)
	}

	setAgentLastSeen(t, s, agent.ID, now)
	events = s.sweepAgentLiveness(now.Add(20 * time.Second))
	if len(events) != 1 || events[0].Kind != livenessEventReconnect {
		t.Fatalf("fresh last_seen reconnect events = %#v, want one reconnect", events)
	}

	setAgentLastSeen(t, s, agent.ID, now.Add(-11*time.Minute))
	events = s.sweepAgentLiveness(now.Add(25 * time.Second))
	if len(events) != 1 || events[0].Kind != livenessEventOffline {
		t.Fatalf("offline after fresh reconnect events = %#v, want one offline", events)
	}

	s.enterWait(agent.ID, "")
	events = s.sweepAgentLiveness(now.Add(30 * time.Second))
	s.leaveWait(agent.ID, "")
	if len(events) != 1 || events[0].Kind != livenessEventReconnect {
		t.Fatalf("reconnect events = %#v, want one reconnect", events)
	}

	setAgentLastSeen(t, s, agent.ID, now.Add(-11*time.Minute))
	events = s.sweepAgentLiveness(now.Add(45 * time.Second))
	if len(events) != 1 || events[0].Kind != livenessEventOffline {
		t.Fatalf("offline after reconnect events = %#v, want one offline", events)
	}
}

func TestSweepAgentLivenessOpenWaitAndWebsocketKeepActive(t *testing.T) {
	s, _ := setupTestServer(t)
	waiter, _, err := s.registerAI("gpt5", "codex", "test", nil, "waitlive")
	if err != nil {
		t.Fatal(err)
	}
	wsAgent, _, err := s.registerAI("gpt5", "codex", "test", nil, "wslive")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	setAgentLastSeen(t, s, waiter.ID, now.Add(-11*time.Minute))
	setAgentLastSeen(t, s, wsAgent.ID, now.Add(-11*time.Minute))

	s.enterWait(waiter.ID, "")
	defer s.leaveWait(waiter.ID, "")
	s.enterWS(wsAgent.ID)
	defer s.leaveWS(wsAgent.ID)

	events := s.sweepAgentLiveness(now)
	if len(events) != 0 {
		t.Fatalf("events = %#v, want none", events)
	}
	if got := findListedAgent(t, s, waiter.ID); got.State != types.AgentStateIdle {
		t.Fatalf("waiter state = %q, want idle", got.State)
	}
	if got := findListedAgent(t, s, wsAgent.ID); got.State != types.AgentStateIdle {
		t.Fatalf("ws state = %q, want idle", got.State)
	}
}

func TestEmitLivenessOfflineTargetsHumansAndNamesRole(t *testing.T) {
	s, _ := setupTestServer(t)
	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "notify")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("humans", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("humans", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("humans", ai.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("humans", ai.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("robots", ai.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("robots", ai.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	setAgentLastSeen(t, s, ai.ID, now.Add(-11*time.Minute))

	s.emitLivenessEvents(s.sweepAgentLiveness(now))

	humanMsg := newestRoomMessage(t, s, "humans")
	if !humanMsg.NeedsHumanAttention {
		t.Fatal("human-room offline message did not set needs attention")
	}
	if len(humanMsg.Targets) != 1 || humanMsg.Targets[0] != human.ID {
		t.Fatalf("targets = %v, want [%s]", humanMsg.Targets, human.ID)
	}
	if !strings.Contains(humanMsg.Body, "worker "+ai.ID) {
		t.Fatalf("body = %q, want role and agent ID", humanMsg.Body)
	}

	robotMsg := newestRoomMessage(t, s, "robots")
	if robotMsg.NeedsHumanAttention {
		t.Fatal("AI-only room offline message unexpectedly needs attention")
	}
	if len(robotMsg.Targets) != 0 {
		t.Fatalf("AI-only room targets = %v, want none", robotMsg.Targets)
	}
}

func TestCleanupStaleAgentsDoesNotEmitDisconnectDuplicate(t *testing.T) {
	s, _ := setupTestServer(t)
	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "pruned")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", ai.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	setAgentLastSeen(t, s, ai.ID, now.Add(-31*time.Minute))

	s.emitLivenessEvents(s.sweepAgentLiveness(now))
	s.cleanupStaleAgents()

	var bodies []string
	s.mu.RLock()
	for _, msg := range s.messages["general"] {
		bodies = append(bodies, msg.Body)
	}
	s.mu.RUnlock()

	var disconnectCount int
	var pruneCount int
	for _, body := range bodies {
		if strings.Contains(body, "looks offline") {
			disconnectCount++
		}
		if strings.Contains(body, "pruned after stale timeout") {
			pruneCount++
		}
	}
	if disconnectCount != 1 {
		t.Fatalf("offline messages = %d, want 1; bodies=%v", disconnectCount, bodies)
	}
	if pruneCount != 1 {
		t.Fatalf("prune messages = %d, want 1; bodies=%v", pruneCount, bodies)
	}
}

func TestSetAgentStateActivityRefreshesLastSeen(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "active")
	if err != nil {
		t.Fatal(err)
	}

	stale := time.Now().UTC().Add(-3 * time.Minute)
	setAgentLastSeen(t, s, agent.ID, stale)

	for _, state := range []string{types.AgentStateThinking, types.AgentStateToolCall} {
		setAgentLastSeen(t, s, agent.ID, stale)
		s.setAgentState(agent.ID, state)

		s.mu.RLock()
		lastSeen, _ := time.Parse(time.RFC3339, s.agents[agent.ID].LastSeen)
		s.mu.RUnlock()

		if time.Since(lastSeen) > 5*time.Second {
			t.Errorf("state %q: last_seen not refreshed (age %v)", state, time.Since(lastSeen))
		}
	}

	// Non-activity states must NOT refresh last_seen.
	for _, state := range []string{types.AgentStateIdle, types.AgentStateRespawning, types.AgentStateError} {
		setAgentLastSeen(t, s, agent.ID, stale)
		s.setAgentState(agent.ID, state)

		s.mu.RLock()
		lastSeen, _ := time.Parse(time.RFC3339, s.agents[agent.ID].LastSeen)
		s.mu.RUnlock()

		if lastSeen.After(stale.Add(time.Second)) {
			t.Errorf("state %q: last_seen unexpectedly refreshed", state)
		}
	}
}

func TestSetAgentStateActivityKeepsAgentAlive(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "keepalive")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// Age the agent into the stale window (but not yet offline).
	setAgentLastSeen(t, s, agent.ID, now.Add(-2*time.Minute))
	events := s.sweepAgentLiveness(now)
	if len(events) != 0 {
		t.Fatalf("expected no events at 2 min, got %v", events)
	}
	got := findListedAgent(t, s, agent.ID)
	if got.State != types.AgentStateStale {
		t.Fatalf("want stale at 2 min, got %q", got.State)
	}

	// A thinking/tool_call state push should refresh last_seen.
	s.setAgentState(agent.ID, types.AgentStateThinking)

	// Sweep again: agent should no longer be stale (last_seen was refreshed).
	s.mu.Lock()
	s.agents[agent.ID].State = ""
	s.agents[agent.ID].StateAt = time.Time{}
	s.mu.Unlock()
	events = s.sweepAgentLiveness(now)
	got = findListedAgent(t, s, agent.ID)
	if got.State == types.AgentStateStale || got.State == types.AgentStateOffline {
		t.Fatalf("agent should not be stale/offline after thinking state push, got %q", got.State)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events after state refresh, got %v", events)
	}
}

func setAgentLastSeen(t *testing.T, s *store, agentID string, ts time.Time) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	if !ok {
		t.Fatalf("agent %s not found", agentID)
	}
	a.LastSeen = ts.Format(time.RFC3339)
}

func newestRoomMessage(t *testing.T, s *store, roomID string) types.Message {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.messages[roomID]
	if len(msgs) == 0 {
		t.Fatalf("room %s has no messages", roomID)
	}
	return msgs[len(msgs)-1]
}
