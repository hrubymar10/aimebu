package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestRoomPreferencesStayWithTheRequestingUser(t *testing.T) {
	f := newFixture(t)
	f.seed()
	secondCookie, secondCSRF := registerSecondUser(t, f, "second")
	second, err := f.db.UserByLogin(t.Context(), "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, second.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/rooms/" + f.room.ID + "/preferences"
	resp := f.request(http.MethodPatch, path, map[string]bool{"hidden": true}, false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF status=%d", resp.StatusCode)
	}
	resp = f.request(http.MethodPatch, path, map[string]bool{"hidden": true}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("owner preference status=%d", resp.StatusCode)
	}
	list := func(cookie *http.Cookie) store.Room {
		t.Helper()
		resp := userRequest(t, f, cookie, "", http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("room list status=%d", resp.StatusCode)
		}
		var rooms []store.Room
		if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
			t.Fatal(err)
		}
		for _, room := range rooms {
			if room.ID == f.room.ID {
				return room
			}
		}
		t.Fatal("task room absent")
		return store.Room{}
	}
	if !list(f.cookie).Hidden || list(secondCookie).Hidden {
		t.Fatal("room preference leaked in HTTP list")
	}
	resp = userRequest(t, f, secondCookie, secondCSRF, http.MethodPatch, path, map[string]bool{"hidden": true})
	resp.Body.Close()
	if resp.StatusCode != 200 || !list(secondCookie).Hidden || !list(f.cookie).Hidden {
		t.Fatal("second user's preference changed owner's view")
	}
}

func TestRoomReadCursorIsServerSideAndMonotonic(t *testing.T) {
	f := newFixture(t)
	f.seed()
	secondCookie, _ := registerSecondUser(t, f, "second")
	second, err := f.db.UserByLogin(t.Context(), "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, second.ID); err != nil {
			return err
		}
		for i, author := range []string{"agt_a", "agt_a", "agt_a"} {
			if _, err := tx.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
				VALUES (?,?,?,'agent',?,'hi','[]','2026-10-16T00:00:00Z')`, "msg_rc"+string(rune('a'+i)), 1000+i, f.room.ID, author); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE seq_counter SET value=2000`); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,created_at)
			VALUES ('inb_rc',?,?,'agent','agt_a','question','now')`, f.project.ID, f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	room := func(cookie *http.Cookie) store.Room {
		t.Helper()
		resp := userRequest(t, f, cookie, "", http.MethodGet, "/api/rooms/"+f.room.ID, nil)
		defer resp.Body.Close()
		var r store.Room
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || resp.StatusCode != 200 {
			t.Fatalf("room status=%d err=%v", resp.StatusCode, err)
		}
		return r
	}
	if r := room(f.cookie); r.UnreadCount != 3 || r.AttentionCount != 1 || r.ReadCursor != 0 {
		t.Fatalf("before read: %+v", r)
	}
	path := "/api/rooms/" + f.room.ID + "/read"
	mark := func(body any) store.ReadState {
		t.Helper()
		resp := f.request(http.MethodPost, path, body, true)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("read status=%d", resp.StatusCode)
		}
		var state store.ReadState
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	if state := mark(map[string]int64{"seq": 1000}); state.ReadCursor != 1000 || state.UnreadCount != 2 {
		t.Fatalf("partial read: %+v", state)
	}
	if state := mark(map[string]int64{"seq": 5}); state.ReadCursor != 1000 {
		t.Fatalf("cursor moved backwards: %+v", state)
	}
	if r := room(secondCookie); r.UnreadCount != 3 {
		t.Fatalf("cursor leaked to another user: %+v", r)
	}
	if state := mark(map[string]int64{}); state.ReadCursor != 1002 || state.UnreadCount != 0 {
		t.Fatalf("read all: %+v", state)
	}
	if r := room(f.cookie); r.UnreadCount != 0 || r.AttentionCount != 1 || r.ReadCursor != 1002 {
		t.Fatalf("after read: %+v", r)
	}
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "mine"}, true)
	var sent store.Message
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil || resp.StatusCode != 201 {
		t.Fatalf("send status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if r := room(f.cookie); r.ReadCursor != sent.Seq || r.UnreadCount != 0 {
		t.Fatalf("sender cursor did not follow own message: %+v sent seq=%d", r, sent.Seq)
	}
	if r := room(secondCookie); r.UnreadCount != 4 {
		t.Fatalf("another user's unread should include the new message: %+v", r)
	}
	resp = f.request(http.MethodPost, path, map[string]int64{"seq": -1}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("negative seq status=%d", resp.StatusCode)
	}
}

func TestRoomReadReceiptsListHumansAndAgents(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	var first, second string
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE seq_counter SET value=3000`); err != nil {
			return err
		}
		for i, id := range []*string{&first, &second} {
			*id = "msg_rr" + string(rune('a'+i))
			if _, err := tx.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
				VALUES (?,?,?,'system','system','hi','[]','2026-10-16T00:00:00Z')`, *id, 2000+i, f.room.ID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note_rr',?,?,?,'done','mention','now')`, agent.ID, first, f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/read", map[string]int64{"seq": 2001}, true)
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/read-receipts", nil, false)
	defer resp.Body.Close()
	var receipts []store.ReadReceipt
	if err := json.NewDecoder(resp.Body).Decode(&receipts); err != nil || resp.StatusCode != 200 {
		t.Fatalf("receipts status=%d err=%v", resp.StatusCode, err)
	}
	seen := map[string]int64{}
	for _, r := range receipts {
		seen[r.MemberKind+":"+r.Label] = r.Seq
	}
	if seen["user:owner"] != 2001 || seen["agent:toolagent"] != 2000 || len(seen) != 2 {
		t.Fatalf("receipts = %+v", receipts)
	}
}

// An agent's marker follows its own replies: after it answers, it has seen everything up to that answer.
func TestAgentReadReceiptAdvancesPastItsOwnReply(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE seq_counter SET value=4000`); err != nil {
			return err
		}
		for _, row := range []struct {
			id, kind, author string
			seq              int
		}{{"msg_ra", "user", "owner", 3000}, {"msg_rb", "agent", agent.ID, 3005}} {
			if _, err := tx.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
				VALUES (?,?,?,?,?,'hi','[]','2026-10-16T00:00:00Z')`, row.id, row.seq, f.room.ID, row.kind, row.author); err != nil {
				return err
			}
		}
		// The agent consumed #3000 in a turn, then answered with #3005.
		_, err := tx.Exec(`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note_rb',?,?,?,'done','mention','now')`, agent.ID, "msg_ra", f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/read-receipts", nil, false)
	defer resp.Body.Close()
	var receipts []store.ReadReceipt
	if err := json.NewDecoder(resp.Body).Decode(&receipts); err != nil || resp.StatusCode != 200 {
		t.Fatalf("receipts status=%d err=%v", resp.StatusCode, err)
	}
	got := int64(-1)
	for _, r := range receipts {
		if r.MemberKind == "agent" && r.MemberID == agent.ID {
			got = r.Seq
		}
	}
	if got != 3005 {
		t.Fatalf("the agent's marker is at %d, want its own reply #3005: %+v", got, receipts)
	}
}

func TestNothingCanBePinned(t *testing.T) {
	f := newFixture(t)
	f.seed()
	// A project list carries no pin, and neither a project nor a room can be pinned any more.
	resp := f.request(http.MethodGet, "/api/projects", nil, false)
	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(raw) == 0 {
		t.Fatal("no projects")
	}
	for _, project := range raw {
		if _, has := project["pinned"]; has {
			t.Fatalf("the project list still carries pinned: %v", project)
		}
	}
	resp = f.request(http.MethodPatch, "/api/projects/"+f.project.ID+"/preferences", map[string]bool{"pinned": true}, true)
	resp.Body.Close()
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("the project pin route still answers: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodPatch, "/api/rooms/"+f.room.ID+"/preferences", map[string]bool{"pinned": true}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("pinning a room status=%d, want 400", resp.StatusCode)
	}
}
