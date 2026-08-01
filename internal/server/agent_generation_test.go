package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestAgentGenerationWindowAndReadTimeMedian(t *testing.T) {
	_, srv := setupTestServer(t)
	postJSONForTest(t, srv, "/agents", map[string]any{
		"kind": "ai", "name": "speedy", "force": true,
		"model": "gpt5", "harness": "codex", "project": "test",
	})
	for seconds := int64(1); seconds <= 11; seconds++ {
		postJSONForTest(t, srv, "/agents/speedy@test/generation", map[string]any{
			"session_id": "thread-a", "generation_ms": seconds * 1000,
		})
	}

	var response struct {
		Agents []types.Agent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agents")), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Agents) != 1 {
		t.Fatalf("agents = %#v", response.Agents)
	}
	agent := response.Agents[0]
	if got := len(agent.GenerationSamplesMS); got != agentGenerationWindow {
		t.Fatalf("sample count = %d, want %d", got, agentGenerationWindow)
	}
	if got := agent.GenerationSamplesMS[0]; got != 2_000 {
		t.Fatalf("oldest retained sample = %d, want 2000", got)
	}
	if got := agent.GenerationMS; got != 6_500 {
		t.Fatalf("generation_ms = %d, want 6500", got)
	}
}

func TestCloneAgentDerivesGenerationMedian(t *testing.T) {
	agent := &types.Agent{GenerationSamplesMS: []int64{60_000, 15_000, 30_000}}
	clone := cloneAgentLocked(agent)
	if got := clone.GenerationMS; got != 30_000 {
		t.Fatalf("cloned generation_ms = %d, want 30000", got)
	}
}

func TestAgentGenerationClearsOnSessionChangeWithReclaimedIdentity(t *testing.T) {
	_, srv := setupTestServer(t)
	register := func(sessionID string) string {
		body := postJSONForTest(t, srv, "/agents", map[string]any{
			"kind": "ai", "model": "gpt5", "harness": "codex", "project": "test",
			"meta":    map[string]string{"spawn_tag": "0123456789abcdef"},
			"session": map[string]string{"harness_session_id": sessionID},
		})
		var response types.RegisterResponse
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		return response.ID
	}

	agentID := register("thread-a")
	for i := 0; i < 3; i++ {
		postJSONForTest(t, srv, fmt.Sprintf("/agents/%s/generation", agentID), map[string]any{
			"session_id": "thread-a", "generation_ms": 1000 + i,
		})
	}

	reclaimedID := register("thread-b")
	if reclaimedID != agentID {
		t.Fatalf("reclaimed id = %q, want %q", reclaimedID, agentID)
	}

	var response struct {
		Agents []types.Agent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agents")), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Agents[0].GenerationSessionID; got != "thread-b" {
		t.Fatalf("generation session = %q, want thread-b", got)
	}
	if got := len(response.Agents[0].GenerationSamplesMS); got != 0 {
		t.Fatalf("samples after reclaimed session change = %v, want empty", response.Agents[0].GenerationSamplesMS)
	}
}

func TestAgentGenerationPersistsAcrossRestart(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "speedy")
	if err != nil {
		t.Fatal(err)
	}
	if !s.recordAgentGeneration(agent.ID, "thread-a", 12_345) {
		t.Fatal("recordAgentGeneration returned false")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	agents := reopened.listAgents()
	if len(agents) != 1 || agents[0].GenerationMS != 12_345 {
		t.Fatalf("agents after restart = %#v, want persisted generation sample", agents)
	}
}
