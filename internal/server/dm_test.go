package server

import (
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestPeopleDMReusesRoomAndKeepsThirdPartiesOut(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	people := make(map[string]struct {
		id, csrf string
		cookie   *http.Cookie
	})
	for _, login := range []string{"alice", "bob", "third"} {
		cookie, csrf := registerSecondUser(t, f, login)
		user, err := f.db.UserByLogin(t.Context(), login)
		if err != nil {
			t.Fatal(err)
		}
		people[login] = struct {
			id, csrf string
			cookie   *http.Cookie
		}{user.ID, csrf, cookie}
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, user.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	alice, bob, third := people["alice"], people["bob"], people["third"]
	path := "/api/projects/" + f.project.ID
	outsideCookie, outsideCSRF := registerSecondUser(t, f, "outside")
	outside, err := f.db.UserByLogin(t.Context(), "outside")
	if err != nil {
		t.Fatal(err)
	}
	resp := f.requestAs(t, outsideCookie, outsideCSRF, http.MethodGet, path+"/dm-peers", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("outside peer list: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodPost, path+"/dms", map[string]string{"user_id": outside.ID})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("outside DM open: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodPost, path+"/dms", map[string]string{"user_id": alice.id})
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("self DM open: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodGet, path+"/dm-peers", nil)
	var peers []store.DMPeer
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(peers) != 3 {
		t.Fatalf("peers: %d %+v", resp.StatusCode, peers)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodPost, path+"/dms", map[string]string{"user_id": bob.id})
	var dm store.Room
	if err := json.NewDecoder(resp.Body).Decode(&dm); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || dm.Kind != "dm" || dm.ProjectID != f.project.ID {
		t.Fatalf("open people DM: %d %+v", resp.StatusCode, dm)
	}
	resp = f.requestAs(t, bob.cookie, bob.csrf, http.MethodPost, path+"/dms", map[string]string{"user_id": alice.id})
	var replay store.Room
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || replay.ID != dm.ID {
		t.Fatalf("reverse open: %d %+v", resp.StatusCode, replay)
	}
	resp = f.requestAs(t, bob.cookie, bob.csrf, http.MethodPost, "/api/rooms/"+dm.ID+"/messages", map[string]string{"body": "hello alice"})
	var message struct {
		ID      string   `json:"id"`
		Targets []string `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || len(message.Targets) != 1 || message.Targets[0] != alice.id {
		t.Fatalf("viewer DM message: %d %+v", resp.StatusCode, message)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodPatch, "/api/rooms/"+dm.ID+"/preferences", map[string]bool{"hidden": true})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("hide DM: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodGet, "/api/rooms/"+dm.ID, nil)
	var detail store.Room
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DM detail: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, alice.cookie, alice.csrf, http.MethodGet, path+"/rooms", nil)
	var listed []store.Room
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var fromList store.Room
	for _, room := range listed {
		if room.ID == dm.ID {
			fromList = room
			break
		}
	}
	if detail.MemberCount != 2 || detail.MessageCount != 1 || !detail.Hidden ||
		detail.MemberCount != fromList.MemberCount || detail.MessageCount != fromList.MessageCount || detail.Hidden != fromList.Hidden {
		t.Fatalf("DM detail differs from room list: detail=%+v list=%+v", detail, fromList)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	for name, peer := range map[string]struct {
		id, csrf string
		cookie   *http.Cookie
	}{"third": third, "owner": {id: owner.ID, cookie: f.cookie, csrf: f.csrf}} {
		resp = f.requestAs(t, peer.cookie, peer.csrf, http.MethodGet, "/api/rooms/"+dm.ID+"/messages", nil)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s read private messages: %d", name, resp.StatusCode)
		}
		resp = f.requestAs(t, peer.cookie, peer.csrf, http.MethodGet, path+"/rooms", nil)
		var rooms []store.Room
		if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, room := range rooms {
			if room.ID == dm.ID {
				t.Fatalf("%s saw private room in list", name)
			}
		}
		events, err := f.db.EventsAfter(t.Context(), peer.id, 0, 1<<60)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.RoomID == dm.ID {
				t.Fatalf("%s saw private event", name)
			}
		}
	}
	resp = f.toolRequest(t, "/api/agent/rooms", key)
	var agentRooms struct {
		Rooms []store.Room `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agentRooms); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, room := range agentRooms.Rooms {
		if room.ID == dm.ID {
			t.Fatalf("agent %s saw private room", agent.ID)
		}
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM membership WHERE project_id=? AND user_id=?`, f.project.ID, bob.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/rooms/" + dm.ID + "/messages", map[string]string{"body": "after departure"}},
		{http.MethodPut, "/api/messages/" + message.ID + "/reactions/%F0%9F%91%8D", nil},
		{http.MethodPost, "/api/rooms/" + dm.ID + "/attachments", nil},
	} {
		resp = f.requestAs(t, alice.cookie, alice.csrf, attempt.method, attempt.path, attempt.body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("write after peer left %s: %d", attempt.path, resp.StatusCode)
		}
	}
}

func TestViewerCannotWriteAgentDM(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
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
	dm, err := f.db.OpenDM(t.Context(), viewer.ID, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: dm.ID, AuthorKind: "system", AuthorID: "system", Body: "private"})
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/rooms/" + dm.ID + "/messages", map[string]string{"body": "run a turn"}},
		{http.MethodPut, "/api/messages/" + message.ID + "/reactions/%F0%9F%91%8D", nil},
		{http.MethodPost, "/api/rooms/" + dm.ID + "/attachments", nil},
		{http.MethodDelete, "/api/rooms/" + dm.ID + "/attachments/staged", nil},
	} {
		resp := f.requestAs(t, cookie, csrf, attempt.method, attempt.path, attempt.body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("viewer wrote agent DM %s: %d", attempt.path, resp.StatusCode)
		}
	}
}

func TestViewerCannotWritePeopleDMAfterAgentJoins(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	cookie, csrf := registerSecondUser(t, f, "viewer")
	viewer, err := f.db.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, viewer.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dm, err := f.db.OpenUserDM(t.Context(), f.project.ID, owner.ID, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), dm.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	resp := f.requestAs(t, cookie, csrf, http.MethodPost, "/api/rooms/"+dm.ID+"/messages", map[string]string{"body": "agent joined"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer wrote people DM with agent member: %d", resp.StatusCode)
	}
}

func TestOpenAgentDMReusesRoomAndDeliversPrivateNote(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	path := "/api/agents/" + agent.ID + "/dm"
	resp := f.request(http.MethodPost, path, nil, true)
	if resp.StatusCode != 200 {
		t.Fatalf("open DM: %d", resp.StatusCode)
	}
	var dm store.Room
	if err := json.NewDecoder(resp.Body).Decode(&dm); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if dm.Kind != "dm" || dm.ProjectID != f.project.ID {
		t.Fatalf("DM=%+v", dm)
	}
	resp = f.request(http.MethodPost, path, nil, true)
	var replay store.Room
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if replay.ID != dm.ID {
		t.Fatalf("duplicate DM: %+v %+v", dm, replay)
	}
	resp = f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, room := range rooms {
		if room.ID == dm.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("DM absent from participant sidebar")
	}
	resp = f.request(http.MethodPost, "/api/rooms/"+dm.ID+"/messages", map[string]string{"body": "hello privately"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("send DM: %d", resp.StatusCode)
	}
	var sent store.Message
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if note, err := f.db.NoteForAgentMessage(t.Context(), agent.ID, sent.ID); err != nil || note.RoomID != dm.ID {
		t.Fatalf("private note=%+v %v", note, err)
	}
	resp = f.toolRequest(t, "/api/agent/rooms", key)
	var agentRooms struct {
		Rooms []store.Room `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agentRooms); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found = false
	for _, room := range agentRooms.Rooms {
		if room.ID == dm.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("DM absent from agent's room list")
	}
}

func TestOwnerCanCloseAndReopenAnotherUsersDM(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at)
			VALUES ('usr_dm_peer','peer','hash','{}','now')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_dm_peer','viewer')`, f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dm, err := f.db.OpenDM(t.Context(), "usr_dm_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/rooms/" + dm.ID
	response := f.request(http.MethodPost, path+"/close", map[string]any{"lifecycle_rev": dm.LifecycleRev}, true)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("owner close=%d", response.StatusCode)
	}
	if _, failures, err := f.db.AdvanceRoomClosures(t.Context(), 0); err != nil || len(failures) != 0 {
		t.Fatalf("advance=%v %v", failures, err)
	}
	closed, err := f.db.RoomByID(t.Context(), dm.ID)
	if err != nil {
		t.Fatal(err)
	}
	response = f.request(http.MethodPost, path+"/reopen", map[string]any{"lifecycle_rev": closed.LifecycleRev}, true)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner reopen=%d", response.StatusCode)
	}
}

// The conversation between two people is one room with one stored title; each of them sees the other's name, not their own next to it.
func TestPeopleDMIsTitledWithTheOtherPerson(t *testing.T) {
	f := newFixture(t)
	f.seed()
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
	open := func(body io.ReadCloser) store.Room {
		var room store.Room
		if err := json.NewDecoder(body).Decode(&room); err != nil {
			t.Fatal(err)
		}
		body.Close()
		return room
	}
	resp := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/dms", map[string]string{"user_id": viewer.ID}, true)
	mine := open(resp.Body)
	if resp.StatusCode != 200 || mine.Title != "viewer" {
		t.Fatalf("opened by the owner: %d %q", resp.StatusCode, mine.Title)
	}
	titleIn := func(rooms []store.Room) string {
		for _, room := range rooms {
			if room.ID == mine.ID {
				return room.Title
			}
		}
		return "(missing)"
	}
	list := func(resp *http.Response) []store.Room {
		var rooms []store.Room
		if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return rooms
	}
	if got := titleIn(list(f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false))); got != "viewer" {
		t.Fatalf("the owner's list: %q", got)
	}
	if got := titleIn(list(f.requestAs(t, cookie, csrf, http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil))); got != "owner" {
		t.Fatalf("the peer's list: %q", got)
	}
}
