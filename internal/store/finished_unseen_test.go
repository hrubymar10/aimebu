package store

import (
	"testing"
	"time"
)

// "Finished, not yet seen" is derived from the person's read cursor: an agent that is not in a turn has written a message for them after
// it, on the room list (the room) and in the agent list (the agent). A message for somebody else, such as a worker's completion for its
// coordinator, does not set it, and neither does a turn still running.
func TestFinishedUnseenFollowsThePersonsReadCursor(t *testing.T) {
	s, room, ctx := notifyFixture(t)
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('m1','host','hash',1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c1','` + room.ProjectID + `','m1','/tmp/test')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('a1','a1','` + room.ProjectID + `','role_worker_default','fake','test','test','fake','m1','c1','hash','owner','now')`,
		`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('s1','a1',0,'fresh_with_history','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := s.AddMember(ctx, room.ID, "agent", "a1"); err != nil {
		t.Fatal(err)
	}
	unseen := func(user string) (bool, bool) {
		t.Helper()
		states, err := s.AgentRoomStates(ctx, room.ID, room.ProjectID, user, time.Now(), time.Hour)
		if err != nil || len(states.Agents) != 1 {
			t.Fatalf("agent states for %s: %+v, %v", user, states, err)
		}
		return notifyView(t, s, ctx, room, user).FinishedUnseen, states.Agents[0].Card.FinishedUnseen
	}
	check := func(user string, room, card bool, why string) {
		t.Helper()
		if r, c := unseen(user); r != room || c != card {
			t.Fatalf("%s: room=%v card=%v for %s, want %v and %v", why, r, c, user, room, card)
		}
	}
	check("one", false, false, "a quiet room")

	// A worker's completion goes to its coordinator, not to the person.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: "a1", Body: "done", OnlyTargets: []string{"someone-else"}}); err != nil {
		t.Fatal(err)
	}
	check("one", false, false, "a message for the coordinator")

	// A message for the person, while the turn is still running, is not a finished turn.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: "a1", Body: "@one the result"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES ('t_run','a1','s1',0,?,1,'normal','running')`, room.ID); err != nil {
		t.Fatal(err)
	}
	check("one", false, false, "a running turn")
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled' WHERE id='t_run'`); err != nil {
		t.Fatal(err)
	}
	check("one", true, true, "the turn ended with a message for the person")
	check("two", false, false, "another person in the room, whom it did not address")

	// Only a read moves the cursor: looking again (a reconnect re-reads the lists) changes nothing, a read of the room clears it, and a
	// cursor that stops short of the message does not.
	check("one", true, true, "a second look")
	var messageSeq int64
	if err := s.db.QueryRow(`SELECT seq FROM message WHERE body='@one the result'`).Scan(&messageSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "two", Body: "later chatter"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRoomRead(ctx, "one", room.ID, messageSeq-1); err != nil {
		t.Fatal(err)
	}
	check("one", true, true, "a cursor short of the agent's message")
	if _, err := s.MarkRoomRead(ctx, "one", room.ID, messageSeq); err != nil {
		t.Fatal(err)
	}
	check("one", false, false, "the person read the message")

	// Mark unread brings it back; a message that asks for attention counts for every person, naming them or not.
	if _, err := s.MarkRoomUnread(ctx, "one", room.ID, messageSeq); err != nil {
		t.Fatal(err)
	}
	check("one", true, true, "marked unread")
	if _, err := s.MarkRoomRead(ctx, "one", room.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: "a1", Body: "a question", NeedsAttention: true}); err != nil {
		t.Fatal(err)
	}
	check("one", true, true, "a message that asks for attention")
	check("two", true, true, "the same message for the other person")
}
