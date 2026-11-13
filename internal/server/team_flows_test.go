package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestTeamFlowAPISetupAndRoomKey(t *testing.T) {
	f := newFixture(t)
	f.seed()
	resp := f.request(http.MethodGet, "/api/team-flows", nil, false)
	var catalog struct {
		Templates []store.TeamFlowTemplate `json:"templates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(catalog.Templates) != 3 {
		t.Fatalf("catalog: %d %+v", resp.StatusCode, catalog)
	}
	path := "/api/projects/" + f.project.ID + "/team-flow"
	resp = f.request(http.MethodGet, path, nil, false)
	var state store.TeamFlowState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || state.Key != "" || state.ReviewPending {
		t.Fatalf("new state: %d %+v", resp.StatusCode, state)
	}
	resp = f.request(http.MethodPost, path+"/preview", map[string]any{"key": "leader-dispatcher"}, true)
	var preview store.TeamFlowPreview
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(preview.CreateRooms) != 1 || preview.CreateRooms[0].Key != "routing" {
		t.Fatalf("preview: %d %+v", resp.StatusCode, preview)
	}
	resp = f.request(http.MethodPut, path, map[string]any{"key": "leader-dispatcher", "rev": 0}, true)
	var applied store.TeamFlowApplied
	if err := json.NewDecoder(resp.Body).Decode(&applied); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || applied.Rev != 1 || len(applied.CreatedRooms) != 1 || applied.CreatedRooms[0].TeamFlowRoomKey != "routing" {
		t.Fatalf("apply: %d %+v", resp.StatusCode, applied)
	}
	resp = f.request(http.MethodPut, path, map[string]any{"key": "custom", "rev": 0}, true)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("stale revision: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodGet, "/api/messages/referenced/1?source_message_id=missing", nil, false)
	var reference store.AgentMessage
	if err := json.NewDecoder(resp.Body).Decode(&reference); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || reference.Available {
		t.Fatalf("missing grant: %d %+v", resp.StatusCode, reference)
	}
}

func TestNewProjectOwnerCanApplyTeamFlowFromCreateResponse(t *testing.T) {
	f := newFixture(t)
	resp := f.request(http.MethodPost, "/api/projects", map[string]string{"name": "new flow", "folder": t.TempDir()}, true)
	var project store.Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || project.Role != "owner" {
		t.Fatalf("create project: %d %+v", resp.StatusCode, project)
	}
	resp = f.request(http.MethodPut, "/api/projects/"+project.ID+"/team-flow", map[string]any{"key": "custom", "rev": 0}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new owner applies flow: %d", resp.StatusCode)
	}
}
