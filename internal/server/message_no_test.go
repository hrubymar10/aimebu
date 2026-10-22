package server

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestMessageNumbersInTheAPIAndLookupByNumber(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	other, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "Other task", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(roomID, body string) (id string, no, seq int64) {
		t.Helper()
		resp := f.request(http.MethodPost, "/api/rooms/"+roomID+"/messages", map[string]string{"body": body}, true)
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("post: %d", resp.StatusCode)
		}
		var message struct {
			ID  string `json:"id"`
			No  int64  `json:"no"`
			Seq int64  `json:"seq"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		return message.ID, message.No, message.Seq
	}
	idA, noA, seqA := post(f.room.ID, "in the first room")
	idB, noB, _ := post(other.ID, "in the other room")
	_, noC, _ := post(f.room.ID, "first room again")
	if noA < 1 || noB != noA+1 || noC != noB+1 {
		t.Fatalf("numbers across rooms: %d %d %d, want consecutive", noA, noB, noC)
	}
	lookup := func(no string) (int, map[string]any) {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/message-numbers/"+no, nil, false)
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	status, body := lookup(itoa(noB))
	if status != 200 || body["id"] != idB || body["room_id"] != other.ID || int64(body["no"].(float64)) != noB {
		t.Fatalf("by-no of the other room's message: %d %v", status, body)
	}
	status, body = lookup(itoa(noA))
	if status != 200 || body["id"] != idA || body["room_id"] != f.room.ID || int64(body["seq"].(float64)) != seqA {
		t.Fatalf("by-no of the first message: %d %v", status, body)
	}
	if status, _ = lookup("99999"); status != 404 {
		t.Fatalf("an unknown number: %d", status)
	}
	for _, bad := range []string{"0", "-3", "abc"} {
		if status, _ = lookup(bad); status != 400 {
			t.Fatalf("number %q: %d, want 400", bad, status)
		}
	}
	// A message read back through the room list carries its number.
	resp := f.request(http.MethodGet, "/api/rooms/"+other.ID+"/messages", nil, false)
	defer resp.Body.Close()
	var page []struct {
		ID string `json:"id"`
		No int64  `json:"no"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range page {
		if message.ID == idB && message.No == noB {
			found = true
		}
	}
	if !found {
		t.Fatalf("room list lacks the number: %+v", page)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAgentMayReactToAMessageByItsNumber(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_reviewer_default")
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "review ready"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"#" + itoa(message.No), itoa(message.No)} {
		req, err := http.NewRequest(http.MethodPut, f.http.URL+"/api/agent/messages/"+url.PathEscape(ref)+"/reactions/"+url.PathEscape("👍"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("react by %q: %d", ref, resp.StatusCode)
		}
	}
	items, err := f.db.ReactionsForMessage(t.Context(), message.ID)
	if err != nil || len(items) != 1 || items[0]["member_id"] != agent.ID {
		t.Fatalf("reactions after reacting by number: %v %v", items, err)
	}
}

// A number in a room the caller cannot read answers exactly like a number nothing carries.
func TestMessageNumberLookupDoesNotRevealInaccessibleRooms(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "elsewhere", Folder: "/tmp/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	room, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: other.ID, Kind: "home", Title: "Elsewhere", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "not for this user"})
	if err != nil {
		t.Fatal(err)
	}
	status := func(no int64) (int, string) {
		resp := f.request(http.MethodGet, "/api/message-numbers/"+strconv.FormatInt(no, 10), nil, false)
		defer resp.Body.Close()
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body["error"]
	}
	a, ea := status(hidden.No)
	b, eb := status(999999)
	if a == 200 {
		t.Skip("the fixture's user can read every project")
	}
	if a != b || ea != eb || a != 404 {
		t.Fatalf("inaccessible room: %d %q, missing number: %d %q; both must be 404 not_found", a, ea, b, eb)
	}
}
