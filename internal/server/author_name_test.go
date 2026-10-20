package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// A message from a person carries the person's login, so the chat can show a name where it only had the account id.
func TestMessagesCarryThePersonsLogin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	registerSecondUser(t, f, "tomastomas")
	other, err := f.db.UserByLogin(t.Context(), "tomastomas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: other.ID, Body: "from tomas"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "notice"}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages", nil, false)
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	names := map[string]any{}
	for _, message := range list {
		names[message["body"].(string)] = message["author_name"]
	}
	if names["from tomas"] != "tomastomas" {
		t.Fatalf("a person's message should carry the login: %v", names)
	}
	if _, ok := names["notice"]; !ok || names["notice"] != nil {
		t.Fatalf("a system notice has no author name: %v", names)
	}
}
