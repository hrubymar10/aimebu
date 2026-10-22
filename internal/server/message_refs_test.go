package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestAgentMessageGetByNumberIsProjectScoped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_reviewer_default")
	child, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, ParentID: f.room.ID, Kind: "task", Title: "roomB", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	inChild, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: child.ID, AuthorKind: "system", AuthorID: "system", Body: "child room text"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "elsewhere", Folder: "/tmp/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: other.ID, Kind: "home", Title: "Elsewhere", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: otherRoom.ID, AuthorKind: "system", AuthorID: "system", Body: "other project secret"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(no string) (int, store.AgentMessage) {
		t.Helper()
		resp := f.toolRequest(t, "/api/agent/message-numbers/"+no, key)
		defer resp.Body.Close()
		var message store.AgentMessage
		_ = json.NewDecoder(resp.Body).Decode(&message)
		return resp.StatusCode, message
	}
	status, message := get(strconv.FormatInt(inChild.No, 10))
	if status != 200 || !message.Available || message.Body != "child room text" || message.Room != "roomB" {
		t.Fatalf("a message of the project's child room: %d %+v", status, message)
	}
	for _, no := range []string{strconv.FormatInt(foreign.No, 10), "999999"} {
		status, message = get(no)
		if status != 200 || message.Available || message.Body != "" || message.Room != "" || message.RoomID != "" || message.From != "" {
			t.Fatalf("number %s from outside the project: %d %+v", no, status, message)
		}
	}
	if status, _ = get("0"); status != http.StatusBadRequest {
		t.Fatalf("number 0: %d", status)
	}
}

// A "#N" in a delivered message puts the message it names into the agent's context, from the project's rooms only.
func TestDeliveryContextCarriesReferencedMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	child, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, ParentID: f.room.ID, Kind: "task", Title: "roomB", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	inChild, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: child.ID, AuthorKind: "system", AuthorID: "system", Body: "decision lives here"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "elsewhere", Folder: "/tmp/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: other.ID, Kind: "home", Title: "Elsewhere", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: otherRoom.ID, AuthorKind: "system", AuthorID: "system", Body: "other project secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='exited', outcome='completed', settle='settled' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := "please look at #" + strconv.FormatInt(inChild.No, 10) + " and #" + strconv.FormatInt(foreign.No, 10)
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": body}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	resp.Body.Close()
	delivery, ok, err := f.db.PrepareDispatch(t.Context(), agent.ID, "")
	if err != nil || !ok {
		t.Fatalf("dispatch ready=%v err=%v", ok, err)
	}
	var context struct {
		Refs []store.AgentMessage `json:"referenced_messages"`
	}
	if err := json.Unmarshal(delivery.Context, &context); err != nil {
		t.Fatal(err)
	}
	if len(context.Refs) != 2 || !context.Refs[0].Available || context.Refs[0].Body != "decision lives here" || context.Refs[0].Room != "roomB" ||
		context.Refs[1].Available || context.Refs[1].Body != "" || context.Refs[1].Room != "" || context.Refs[1].No != foreign.No {
		t.Fatalf("referenced messages in the context: %+v\n%s", context.Refs, delivery.Context)
	}
	if len(delivery.Notes) == 0 || delivery.Notes[len(delivery.Notes)-1].No < 1 {
		t.Fatalf("the delivered note carries no number: %+v", delivery.Notes)
	}
}
