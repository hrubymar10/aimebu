package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (f *fixture) projectMember(t *testing.T, login, projectID, role string) (*http.Cookie, string) {
	t.Helper()
	cookie, csrf := registerSecondUser(t, f, login)
	user, err := f.db.UserByLogin(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, projectID, user.ID, role); err != nil {
		t.Fatal(err)
	}
	return cookie, csrf
}

// People find a chat between agents through the list of chats they could join, read it after joining, and may write only with the
// right role in one of the agents' projects.
func TestPeopleJoinAndWriteInAChatBetweenAgents(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, _ := f.createToolAgent(t, "role_lead_default")
	p2, peer := f.otherProjectAgent(t)
	var chat store.Room
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) (err error) {
		chat, _, err = tx.OpenAgentDM(lead.ID, peer.ID, lead.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: chat.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "secret between agents"}); err != nil {
		t.Fatal(err)
	}
	viewerCookie, viewerCSRF := f.projectMember(t, "viewer", f.project.ID, "viewer")
	operatorCookie, operatorCSRF := f.projectMember(t, "operator", p2.ID, "operator")
	strangerCookie, strangerCSRF := registerSecondUser(t, f, "stranger")

	type listing struct {
		ProjectID string `json:"project_id"`
		Rooms     []store.Room
		Joinable  []store.AgentDMOffer
	}
	list := func(cookie *http.Cookie, csrf string) listing {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, "GET", "/api/agent-dms", nil)
		defer resp.Body.Close()
		var out listing
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
			t.Fatalf("list status=%d", resp.StatusCode)
		}
		return out
	}
	status := func(cookie *http.Cookie, csrf, method, path string, body any) int {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, method, path, body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := list(strangerCookie, strangerCSRF); len(got.Rooms)+len(got.Joinable) != 0 {
		t.Fatalf("a stranger is offered a chat: %+v", got)
	}
	if got := status(strangerCookie, strangerCSRF, "POST", "/api/rooms/"+chat.ID+"/join", nil); got != 404 {
		t.Fatalf("a stranger joined: %d", got)
	}
	if got := status(strangerCookie, strangerCSRF, "GET", "/api/rooms/"+chat.ID+"/messages", nil); got == 200 {
		t.Fatal("a stranger read the chat")
	}
	for name, who := range map[string]struct {
		cookie *http.Cookie
		csrf   string
		write  int
	}{
		"viewer of one agent's project":   {viewerCookie, viewerCSRF, 403},
		"operator of the other's project": {operatorCookie, operatorCSRF, 201},
		"the instance owner":              {f.cookie, f.csrf, 201},
	} {
		before := list(who.cookie, who.csrf)
		if len(before.Rooms) != 0 || len(before.Joinable) != 1 || before.Joinable[0].ID != chat.ID || before.ProjectID != store.AgentDMProjectID {
			t.Fatalf("%s before joining: %+v", name, before)
		}
		if got := status(who.cookie, who.csrf, "GET", "/api/rooms/"+chat.ID+"/messages", nil); got == 200 {
			t.Fatalf("%s read the chat without joining", name)
		}
		if got := status(who.cookie, who.csrf, "POST", "/api/rooms/"+chat.ID+"/join", nil); got != 200 {
			t.Fatalf("%s join: %d", name, got)
		}
		after := list(who.cookie, who.csrf)
		if len(after.Rooms) != 1 || after.Rooms[0].ID != chat.ID || len(after.Joinable) != 0 {
			t.Fatalf("%s after joining: %+v", name, after)
		}
		if got := status(who.cookie, who.csrf, "GET", "/api/rooms/"+chat.ID+"/messages", nil); got != 200 {
			t.Fatalf("%s read after joining: %d", name, got)
		}
		if got := status(who.cookie, who.csrf, "POST", "/api/rooms/"+chat.ID+"/messages", map[string]string{"body": "hello from " + name}); got != who.write {
			t.Fatalf("%s write: %d want %d", name, got, who.write)
		}
	}
	// A person's message in the chat wakes its agents.
	queued, err := f.db.QueuedAgentIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	woken := map[string]bool{}
	for _, id := range queued {
		woken[id] = true
	}
	if !woken[lead.ID] || !woken[peer.ID] {
		t.Fatalf("a person's message did not wake both agents: %v", queued)
	}
	if got := status(operatorCookie, operatorCSRF, "POST", "/api/rooms/"+chat.ID+"/leave", nil); got != 200 {
		t.Fatalf("leave: %d", got)
	}
	if got := status(operatorCookie, operatorCSRF, "GET", "/api/rooms/"+chat.ID+"/messages", nil); got == 200 {
		t.Fatal("read after leaving")
	}
	// Losing every involved project takes the chat away from someone who is still a member of the room.
	if _, err := f.exec(`DELETE FROM membership WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(viewerCookie, viewerCSRF, "GET", "/api/rooms/"+chat.ID+"/messages", nil); got == 200 {
		t.Fatal("a person removed from the agent's project still reads the chat")
	}
}

// Every route that names the reserved project answers as for a project that does not exist, even for the owner.
func TestReservedProjectIsNotReachableByAnyRoute(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/projects/" + store.AgentDMProjectID + "/archive", nil},
		{"POST", "/api/projects/" + store.AgentDMProjectID + "/reopen", nil},
		{"DELETE", "/api/projects/" + store.AgentDMProjectID, map[string]string{"name": "Agent conversations"}},
		{"PATCH", "/api/projects/" + store.AgentDMProjectID, map[string]string{"name": "mine"}},
		{"GET", "/api/projects/" + store.AgentDMProjectID + "/rooms", nil},
		{"PUT", "/api/projects/" + store.AgentDMProjectID + "/members/" + "usr_x", map[string]string{"role": "owner"}},
	} {
		resp := f.request(request.method, request.path, request.body, request.method != "GET")
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s %s: %d", request.method, request.path, resp.StatusCode)
		}
	}
}
