package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
)

func TestReviewerSpawnError(t *testing.T) {
	w := httptest.NewRecorder()
	spawnError(w, store.ErrReviewerAgentsDisabled)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"reviewer_agents_disabled"`) || !strings.Contains(w.Body.String(), "Reviewer agents are disabled for this project") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSetRoleRejectsDisabledReviewer(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, leadKey := f.createToolAgentNamed(t, "role_leader_default", "reviewlead")
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "reviewworker")
	if err := f.db.SetGlobalSettingValue(t.Context(), store.ReviewerAgentsGlobalKey, "0"); err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/agents/"+worker.ID+"/role", leadKey, "blocked-reviewer-role", map[string]any{"role_key": "reviewer", "rev": 0})
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 409 || !strings.Contains(string(body), `"reviewer_agents_disabled"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}
