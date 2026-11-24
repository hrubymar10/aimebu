package store

import (
	"context"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A10-3b: rooms are referenced by id. A person's typed `#<title>` is resolved
// to `#<room id>` on save among the rooms the author can see; an agent writes
// the id itself; the entities table names a referenced room only when the
// reader may see it, and never leaks the title of one it may not.

func roomRefSeed(t *testing.T, s *Store) {
	t.Helper()
	mentionSeed(t, s)
}

func restrictedRoom(t *testing.T, s *Store, title string) Room {
	t.Helper()
	room := mentionRoom(t, s, "task", title)
	if _, err := s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	return room
}

// A person's typed `#<title>` resolves to the room's id on save; everything
// that is not one visible room's exact title stays as it was written.
func TestRoomRefStoredAsID(t *testing.T) {
	s, _ := testStore(t, Options{})
	roomRefSeed(t, s)
	general := mentionRoom(t, s, "task", "General")

	message := mentionPersonMessage(t, s, general.ID, "see #General for the plan")
	if stored := storedBody(t, s, message.ID); !strings.Contains(stored, "#"+general.ID) || strings.Contains(stored, "#General") {
		t.Fatalf("stored body %q is not stored by room ID", stored)
	}

	// The canonical id form a person typed stays as it was written.
	direct := mentionPersonMessage(t, s, general.ID, "see #"+general.ID+" again")
	if stored := storedBody(t, s, direct.ID); !strings.Contains(stored, "#"+general.ID) {
		t.Fatalf("a typed room id was rewritten: %q", stored)
	}

	// A message number is never a room: the existing #N reference stays.
	numbered := mentionPersonMessage(t, s, general.ID, "context in #1 and #nope")
	if stored := storedBody(t, s, numbered.ID); !strings.Contains(stored, "#1 ") || strings.Contains(stored, "#room_") {
		t.Fatalf("a message number or an unknown title was rewritten: %q", stored)
	}

	// Code never resolves.
	code := mentionPersonMessage(t, s, general.ID, "run `#General` first")
	if stored := storedBody(t, s, code.ID); strings.Contains(stored, "#"+general.ID) {
		t.Fatalf("a room title inside code was resolved: %q", stored)
	}

	// Two visible rooms share the title: ambiguous, the text stays.
	twin := mentionRoom(t, s, "task", "General")
	ambiguous := mentionPersonMessage(t, s, general.ID, "see #General now")
	if stored := storedBody(t, s, ambiguous.ID); strings.Contains(stored, "#"+general.ID) || strings.Contains(stored, "#"+twin.ID) {
		t.Fatalf("an ambiguous title was resolved: %q", stored)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed',closed_at=? WHERE id=?`, timestamp(), twin.ID); err != nil {
		t.Fatal(err)
	}

	// A room of another project is not among the rooms the author can see.
	other, err := s.CreateProject(context.Background(), Project{Name: "sable", Folder: "/tmp/sable"})
	if err != nil {
		t.Fatal(err)
	}
	annexe, err := s.CreateRoom(context.Background(), Room{ProjectID: other.ID, Kind: "task", Title: "Annexe", CreatedBy: "usr_pia"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := mentionPersonMessage(t, s, general.ID, "see #Annexe over there")
	if stored := storedBody(t, s, foreign.ID); strings.Contains(stored, annexe.ID) {
		t.Fatalf("an invisible room's title was resolved: %q", stored)
	}

	// An agent writes ids: a typed title of an agent stays as it was written.
	wren := mentionAgent(t, s, "wren", general.ID)
	agentMessage, err := s.InsertMessage(context.Background(), Message{RoomID: general.ID, AuthorKind: "agent", AuthorID: wren.ID, Body: "the room #General holds the plan"})
	if err != nil {
		t.Fatal(err)
	}
	if stored := storedBody(t, s, agentMessage.ID); strings.Contains(stored, "#"+general.ID) {
		t.Fatalf("an agent's typed title was resolved: %q", stored)
	}
}

// The entities table names a referenced room only when the reading agent may
// see it; an unknown or deleted id is left out entirely.
func TestRoomRefEntitiesVisibility(t *testing.T) {
	s, _ := testStore(t, Options{})
	roomRefSeed(t, s)
	home := mentionRoom(t, s, "task", "wiring")
	wren := mentionAgent(t, s, "wren", home.ID)
	vault := restrictedRoom(t, s, "vault") // same project, wren is not a member
	other, err := s.CreateProject(context.Background(), Project{Name: "sable", Folder: "/tmp/sable"})
	if err != nil {
		t.Fatal(err)
	}
	hall, err := s.CreateRoom(context.Background(), Room{ProjectID: other.ID, Kind: "task", Title: "Hall", CreatedBy: "usr_pia"})
	if err != nil {
		t.Fatal(err)
	}

	message := mentionPersonMessage(t, s, home.ID, "moved to #"+home.ID+" from #"+vault.ID+" and #"+hall.ID+" and #room_01unknown")
	byNo, err := s.AgentMessageByNo(context.Background(), wren.ID, "prj_mentions", message.No)
	if err != nil {
		t.Fatal(err)
	}
	var rooms []protocol.Entity
	for _, entity := range byNo.Entities {
		if entity.Kind == "room" {
			rooms = append(rooms, protocol.Entity{ID: entity.ID, Name: entity.Name, Kind: "room"})
		}
	}
	if len(rooms) != 3 {
		t.Fatalf("room entities = %+v, want the readable room named, the private and the foreign one unnamed, and the unknown id left out", rooms)
	}
	if rooms[0].ID != home.ID || rooms[0].Name != "wiring" {
		t.Fatalf("a readable room lost its title: %+v", rooms[0])
	}
	if rooms[1].ID != vault.ID || rooms[1].Name != "" {
		t.Fatalf("a restricted room the reader cannot see leaked a title: %+v", rooms[1])
	}
	if rooms[2].ID != hall.ID || rooms[2].Name != "" {
		t.Fatalf("another project's room leaked a title: %+v", rooms[2])
	}

	page, err := s.AgentRoomHistory(context.Background(), wren.ID, "prj_mentions", home.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) == 0 || len(page.Messages[0].Entities) == 0 {
		t.Fatalf("room_history carries no room entities: %+v", page.Messages)
	}
}
