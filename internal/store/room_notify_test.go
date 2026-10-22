package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func notifyFixture(t *testing.T) (*Store, Room, context.Context) {
	t.Helper()
	s, _ := testStore(t, Options{})
	project, room, _ := fixture(t, s)
	ctx := context.Background()
	for _, user := range []string{"one", "two"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user, user); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, user); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, room.ID, "user", user); err != nil {
			t.Fatal(err)
		}
	}
	return s, room, ctx
}

func notifyView(t *testing.T, s *Store, ctx context.Context, room Room, user string) Room {
	t.Helper()
	rooms, err := s.RoomsForUserProject(ctx, room.ProjectID, user)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms for %s: %+v, %v", user, rooms, err)
	}
	return rooms[0]
}

func TestRoomNotificationPreferenceDefaultsAndIsPrivateToThePerson(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	if view := notifyView(t, s, ctx, room, "one"); view.Notify != "default" || view.MutedUntil != "" {
		t.Fatalf("a room nobody set up: notify=%q muted_until=%q", view.Notify, view.MutedUntil)
	}
	level := "mentions"
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	preference, err := s.SetRoomViewPreference(ctx, "one", room.ID, RoomViewUpdate{Notify: &level, MutedUntil: &until})
	if err != nil || preference.Notify != "mentions" || preference.MutedUntil == "" || preference.Hidden {
		t.Fatalf("set: %+v, %v", preference, err)
	}
	one, two := notifyView(t, s, ctx, room, "one"), notifyView(t, s, ctx, room, "two")
	if one.Notify != "mentions" || one.MutedUntil != preference.MutedUntil || two.Notify != "default" || two.MutedUntil != "" {
		t.Fatalf("one=%+v two=%+v: the setting leaked between people", one, two)
	}
	// A request that changes one thing keeps the others, and an empty time ends the mute.
	hidden, none, cleared := true, "none", ""
	if preference, err = s.SetRoomViewPreference(ctx, "one", room.ID, RoomViewUpdate{Hidden: &hidden}); err != nil || preference.Notify != "mentions" || preference.MutedUntil == "" || !preference.Hidden {
		t.Fatalf("hide kept: %+v, %v", preference, err)
	}
	if preference, err = s.SetRoomViewPreference(ctx, "one", room.ID, RoomViewUpdate{Notify: &none, MutedUntil: &cleared}); err != nil || preference.Notify != "none" || preference.MutedUntil != "" || !preference.Hidden {
		t.Fatalf("level and unmute: %+v, %v", preference, err)
	}
}

func TestRoomNotificationPreferenceRefusesWhatItCannotKeep(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	bad := "loud"
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	far := time.Now().Add(2 * 366 * 24 * time.Hour).UTC().Format(time.RFC3339)
	words := "tomorrow"
	for name, update := range map[string]RoomViewUpdate{"level": {Notify: &bad}, "past": {MutedUntil: &past}, "too far": {MutedUntil: &far}, "not a time": {MutedUntil: &words}} {
		if _, err := s.SetRoomViewPreference(ctx, "one", room.ID, update); !errors.Is(err, ErrInvalidRoomPreference) {
			t.Fatalf("%s: err = %v, want ErrInvalidRoomPreference", name, err)
		}
	}
	if view := notifyView(t, s, ctx, room, "one"); view.Notify != "default" || view.MutedUntil != "" {
		t.Fatalf("a refused request changed the room: %+v", view)
	}
}

// With "mentions" the badge counts only what addresses the person (a mention, a reply, a group they are in), and a read mark answers the same
// count; every other level counts all new messages as before.
func TestMentionsOnlyRoomCountsOnlyAddressedMessagesAsUnread(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	say := func(body, reply string) Message {
		t.Helper()
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: body, ReplyTo: reply})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	own, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "one", Body: "my question"})
	if err != nil {
		t.Fatal(err)
	}
	say("ordinary update", "")
	say("another one", "")
	say("@one please look", "")
	say("a reply to you", own.ID)
	if got := notifyView(t, s, ctx, room, "one").UnreadCount; got != 4 {
		t.Fatalf("default level unread = %d, want 4", got)
	}
	mentions := "mentions"
	if _, err := s.SetRoomViewPreference(ctx, "one", room.ID, RoomViewUpdate{Notify: &mentions}); err != nil {
		t.Fatal(err)
	}
	if got := notifyView(t, s, ctx, room, "one").UnreadCount; got != 2 {
		t.Fatalf("mentions level unread = %d, want 2 (the mention and the reply)", got)
	}
	if got := notifyView(t, s, ctx, room, "two").UnreadCount; got != 0 {
		t.Fatalf("someone else's unread = %d (their own writing is read)", got)
	}
	state, err := s.MarkRoomRead(ctx, "one", room.ID, own.Seq+1)
	if err != nil || state.UnreadCount != 2 {
		t.Fatalf("read mark in a mentions room: %+v, %v (the answer must match the list)", state, err)
	}
	state, err = s.MarkRoomRead(ctx, "one", room.ID, 0)
	if err != nil || state.UnreadCount != 0 {
		t.Fatalf("read to the newest: %+v, %v", state, err)
	}
}

func TestMessageCreatedEventNamesTheAuthorAndWhoItAddresses(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: "@one ping"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: "nobody in particular"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.EventsAfter(ctx, "one", 0, 1<<60)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]any{}
	for _, event := range events {
		if event.Kind != "message_created" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		seen[payload["message_id"].(string)] = payload
	}
	addressed, ok := seen[message.ID]
	if !ok || addressed["author_kind"] != "user" || addressed["author_id"] != "two" || !strings.Contains(string(mustJSON(t, addressed["targets"])), `"one"`) {
		t.Fatalf("addressed message event = %+v", addressed)
	}
	if targets, ok := seen[plain.ID]["targets"].([]any); !ok || len(targets) != 0 {
		t.Fatalf("an unaddressed message names targets %+v (want an empty list, not null)", seen[plain.ID]["targets"])
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A room that no list has described (just created, or answered by a tool) still names its level; and an explicit level is kept.
func TestRoomAlwaysSerialisesANotificationLevel(t *testing.T) {
	for level, want := range map[string]string{"": "default", "all": "all", "none": "none"} {
		data, err := json.Marshal(Room{ID: "room_x", Notify: level})
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil || out["notify"] != want || out["id"] != "room_x" {
			t.Fatalf("level %q serialised as %s (%v)", level, data, err)
		}
	}
}
