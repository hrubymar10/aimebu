package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func TestAgentSessionRegistrySurvivesStalePruneAndClearAllWipes(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAIWithSession("gpt5", "codex", "alpha", map[string]string{"cwd": "/work"}, "alice", &types.AgentSession{
		HarnessSessionID: "thread-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.agents[agent.ID].LastSeen = time.Now().UTC().Add(-31 * time.Minute).Format(time.RFC3339)
	s.persist()
	s.mu.Unlock()

	s.cleanupStaleAgents()
	if _, ok := s.agents[agent.ID]; ok {
		t.Fatal("stale agent was not pruned")
	}
	if got := s.listAgentSessions(); len(got) != 1 || got[0].FullID != agent.ID {
		t.Fatalf("agent sessions after stale prune = %#v, want row for %s", got, agent.ID)
	}

	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.listAgentSessions(); len(got) != 1 || got[0].HarnessSessionID != "thread-1" {
		t.Fatalf("agent sessions after restart = %#v, want thread-1", got)
	}

	reopened.clearAll(false)
	if got := reopened.listAgentSessions(); len(got) != 0 {
		t.Fatalf("agent sessions after clearAll(false) = %#v, want empty", got)
	}
}

func TestAgentSessionHTTPRegisterPushAndList(t *testing.T) {
	_, srv := setupTestServer(t)

	register := postJSONForTest(t, srv, "/agents", map[string]any{
		"kind":    "ai",
		"name":    "alpha",
		"force":   true,
		"model":   "gpt5",
		"harness": "codex",
		"project": "proj-a",
		"session": map[string]any{"harness_session_id": "thread-a", "cwd": "/repo/a"},
	})
	if !strings.Contains(register, `"reclaimed":false`) {
		t.Fatalf("register response = %s", register)
	}

	var listed struct {
		AgentSessions []types.AgentSession `json:"agent_sessions"`
	}
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agent-sessions")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.AgentSessions) != 1 || listed.AgentSessions[0].Origin != "mcp" || listed.AgentSessions[0].HarnessSessionID != "thread-a" {
		t.Fatalf("agent_sessions = %#v, want mcp thread-a", listed.AgentSessions)
	}

	postJSONForTest(t, srv, "/agents/alpha@proj-a/session", map[string]any{
		"harness_session_id": "thread-b",
		"resume_command":     "aimebu agent --resume-id thread-b -- codex",
	})
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agent-sessions")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.AgentSessions) != 1 || listed.AgentSessions[0].Origin != "wrapper" || listed.AgentSessions[0].HarnessSessionID != "thread-b" {
		t.Fatalf("agent_sessions after wrapper push = %#v, want wrapper thread-b", listed.AgentSessions)
	}
}

func TestAgentSessionHTTPRejectsMalformedHint(t *testing.T) {
	_, srv := setupTestServer(t)

	body := strings.Repeat("x", agentSessionFieldMax+1)
	resp := doJSONForTestExpectStatus(t, srv, "POST", "/agents", map[string]any{
		"kind":    "ai",
		"model":   "gpt5",
		"harness": "codex",
		"session": map[string]any{"harness_session_id": body},
	}, 400)
	if !strings.Contains(resp, "session.harness_session_id") {
		t.Fatalf("error response = %s", resp)
	}
}

func doJSONForTestExpectStatus(t *testing.T, srv *httptest.Server, method, path string, body any, want int) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s status %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	return string(out)
}
