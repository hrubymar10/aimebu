package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestRoomCloseAndReopenAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID
	resp := f.request(http.MethodPost, path+"/close", map[string]any{"lifecycle_rev": 1}, true)
	if resp.StatusCode != 409 {
		t.Fatalf("stale close status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, path+"/close", map[string]any{"lifecycle_rev": 0}, true)
	var closing store.Room
	if err := json.NewDecoder(resp.Body).Decode(&closing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 || closing.Lifecycle != "closing" || closing.LifecycleRev != 1 {
		t.Fatalf("close status=%d room=%+v", resp.StatusCode, closing)
	}
	if _, failures, err := f.db.AdvanceRoomClosures(t.Context(), 30*time.Second); err != nil || len(failures) != 0 {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	var closed store.Room
	if err := json.NewDecoder(resp.Body).Decode(&closed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if closed.Lifecycle != "closed" || closed.ClosedAt == "" || closed.LifecycleRev != 2 {
		t.Fatalf("closed room=%+v", closed)
	}
	resp = f.request(http.MethodPost, path+"/reopen", map[string]any{"lifecycle_rev": 2}, true)
	var reopened store.Room
	if err := json.NewDecoder(resp.Body).Decode(&reopened); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || reopened.Lifecycle != "open" || reopened.Gate != "open" || reopened.ClosedAt != "" || reopened.LifecycleRev != 3 {
		t.Fatalf("reopen status=%d room=%+v", resp.StatusCode, reopened)
	}
}

func TestRoomLifecyclePermissionErrorIsForbidden(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	roomLifecycleError(w, store.ErrForbidden)
	if w.Code != http.StatusForbidden {
		t.Fatalf("permission status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestOnlyTheProjectOwnerRemovesAnArchivedTaskRoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID
	status := func(resp *http.Response) int { resp.Body.Close(); return resp.StatusCode }
	// An operator may archive but not remove.
	cookie, csrf := registerSecondUser(t, f, "operator")
	operator, err := f.db.UserByLogin(t.Context(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, operator.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, path+"/close", map[string]any{"lifecycle_rev": 0}, true)
	resp.Body.Close()
	if _, failures, err := f.db.AdvanceRoomClosures(t.Context(), 30*time.Second); err != nil || len(failures) != 0 {
		t.Fatal(err)
	}
	if got := status(f.requestAs(t, cookie, csrf, http.MethodDelete, path, nil)); got != 403 {
		t.Fatalf("an operator must not remove a room: %d", got)
	}
	if got := status(f.request(http.MethodGet, path, nil, false)); got != 200 {
		t.Fatalf("the refused removal deleted the room: %d", got)
	}
	// The home room is never removed, even by the owner.
	rooms := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var list []store.Room
	if err := json.NewDecoder(rooms.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	rooms.Body.Close()
	for _, room := range list {
		if room.Kind == "home" {
			if got := status(f.request(http.MethodDelete, "/api/rooms/"+room.ID, nil, true)); got != 409 {
				t.Fatalf("home room removal: %d", got)
			}
		}
	}
	if got := status(f.request(http.MethodDelete, path, nil, false)); got != 403 && got != 401 {
		t.Fatalf("a removal without the CSRF token: %d", got)
	}
	if got := status(f.request(http.MethodDelete, path, nil, true)); got != 204 {
		t.Fatalf("the owner's removal: %d", got)
	}
	if got := status(f.request(http.MethodGet, path, nil, false)); got != 404 {
		t.Fatalf("the removed room is still readable: %d", got)
	}
}

func TestOwnerDeletesOpenTaskRoomDirectly(t *testing.T) {
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID
	resp := f.request(http.MethodDelete, path, nil, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete open task room: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted room still exists: %d", resp.StatusCode)
	}
}

func TestProjectListCarriesTheCallersRole(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request(http.MethodGet, "/api/projects", nil, false)
	defer resp.Body.Close()
	var projects []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0]["role"] != "owner" {
		t.Fatalf("projects: %+v", projects)
	}
}

// The room's message list says which messages expiry may leave out of an agent's context: AI group chatter and broadcasts
// only (not a human's message, a reply, or anything in a private chat).
func TestMessageListFlagsOnlyAIGroupChatterAsExpirable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	f.createToolAgentNamed(t, "role_worker_default", "listener") // a second agent, so the group tags reach someone
	post := func(kind, author, body, replyTo string) store.Message {
		m, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: kind, AuthorID: author, Body: body, ReplyTo: replyTo})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	chatter := post("agent", agent.ID, "@channel status update", "")
	plain := post("agent", agent.ID, "just talking", "") // names nobody: a broadcast, room-wide like a group tag
	human := post("user", "owner", "@channel everyone look", "")
	reply := post("agent", agent.ID, "@ais answer", human.ID)
	mention := post("agent", agent.ID, "please look @owner", "")
	both := post("agent", agent.ID, "@channel and @owner look", "") // the agent gets it only through @channel; @owner is a person
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages?limit=50", nil, false)
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, m := range list {
		got[m["id"].(string)] = m["expirable"]
	}
	want := map[string]any{chatter.ID: true, plain.ID: true, human.ID: false, reply.ID: false, mention.ID: false, both.ID: true}
	for id, flag := range want {
		if got[id] != flag {
			t.Errorf("message %s expirable = %v, want %v (all: %v)", id, got[id], flag, got)
		}
	}
}

// A handoff to the lead and an answer sent to named agents carry @channel in their text, but dispatch gives
// them direct notes, so the message list must not flag them.
func TestMessageListDoesNotFlagHandoffsOrInboxAnswers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	lead, _ := f.createToolAgentNamed(t, "role_leader_default", "theleadagent")
	post := func(m store.Message) store.Message {
		m.RoomID = f.room.ID
		out, err := f.db.InsertMessage(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	plain := post(store.Message{AuthorKind: "agent", AuthorID: agent.ID, Body: "@channel status"})
	handoff := post(store.Message{AuthorKind: "agent", AuthorID: agent.ID, Body: "@channel finished", Handoff: true})
	targeted := post(store.Message{AuthorKind: "agent", AuthorID: agent.ID, Body: "@channel answer", OnlyTargets: []string{lead.ID}})
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages?limit=50", nil, false)
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, m := range list {
		got[m["id"].(string)] = m["expirable"]
	}
	if got[plain.ID] != true || got[handoff.ID] != false || got[targeted.ID] != false {
		t.Fatalf("expirable flags: plain=%v handoff=%v targeted=%v", got[plain.ID], got[handoff.ID], got[targeted.ID])
	}
}
