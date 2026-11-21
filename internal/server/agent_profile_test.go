package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestAgentProfileShowsWhereTheAgentRunsAndWhoStartedIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,native_session_id,native_session_started_at,start_mode,started_at) VALUES ('ses_p',?,0,'3b241101-e2bb-4255-8caf-4136c566a962','2026-10-17T10:00:00.000000000Z','native_resume','2026-10-17T10:00:00.000000000Z')`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	get := func(room, agentID string) (int, store.AgentProfile) {
		resp := f.request(http.MethodGet, "/api/rooms/"+room+"/agents/"+agentID+"/profile", nil, false)
		defer resp.Body.Close()
		var profile store.AgentProfile
		_ = json.NewDecoder(resp.Body).Decode(&profile)
		return resp.StatusCode, profile
	}
	code, p := get(f.room.ID, agent.ID)
	if code != 200 || p.AgentID != agent.ID || p.NativeSessionID != "3b241101-e2bb-4255-8caf-4136c566a962" || p.StartMode != "native_resume" {
		t.Fatalf("profile: %d %+v", code, p)
	}
	if p.Cwd != "/tmp/toolbox" || p.ProjectName != "p" || p.SpawnedBy != "test" || p.MachineName == "" || p.TurnsToday != 0 {
		t.Fatalf("where it runs and who started it: %+v", p)
	}
	if want := "cd /tmp/toolbox && claude --resume 3b241101-e2bb-4255-8caf-4136c566a962"; p.ResumeCommand != want {
		t.Fatalf("resume command = %q, want %q", p.ResumeCommand, want)
	}
	if code, _ := get(f.room.ID, "agt_nobody"); code != 404 {
		t.Fatalf("an agent that is not in the room: %d", code)
	}
	if code, _ := get("room_nobody", agent.ID); code != 404 {
		t.Fatalf("a room that does not exist: %d", code)
	}
	anon, err := http.Get(f.http.URL + "/api/rooms/" + f.room.ID + "/agents/" + agent.ID + "/profile")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Fatalf("anonymous: %d", anon.StatusCode)
	}
}

func TestResumeCommandOnlyForToolsWithKnownSyntax(t *testing.T) {
	// The store keeps the builder private; the HTTP result above covers claude. Unknown tools give none.
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,native_session_id,start_mode,started_at) VALUES ('ses_q',?,0,'abc','native_resume','2026-10-17T10:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	profile, err := f.db.AgentProfile(t.Context(), f.room.ID, agent.ID)
	if err != nil || profile.NativeSessionID != "abc" || profile.ResumeCommand != "" {
		t.Fatalf("a fake tool has no known resume syntax: %+v %v", profile, err)
	}
}

func TestAProjectViewerDoesNotGetTheAgentsHostDetails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,native_session_id,start_mode,started_at) VALUES ('ses_v',?,0,'secret-session','native_resume','2026-10-17T10:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := registerSecondUser(t, f, "viewer")
	viewer, err := f.db.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, viewer.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/rooms/"+f.room.ID+"/agents/"+agent.ID+"/profile", nil)
	defer resp.Body.Close()
	var got store.AgentProfile
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || got.AgentID != agent.ID || got.ProjectName != "p" {
		t.Fatalf("a viewer still reads the profile: %d %+v", resp.StatusCode, got)
	}
	if got.NativeSessionID != "" || got.ResumeCommand != "" || got.Cwd != "" {
		t.Fatalf("a viewer must not get session id, resume command or cwd: %+v", got)
	}
}
